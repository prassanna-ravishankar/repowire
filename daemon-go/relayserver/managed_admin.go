package relayserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Admin figures: the Worker's in-app meter (gross list price per owner) plus,
// when an Account Analytics Read token is configured, Cloudflare's own account
// usage priced after the Workers Paid allowances. Prices were read on
// 2026-10-25; see the managed metering note for sources.

const (
	cfGraphQLEndpoint = "https://api.cloudflare.com/client/v4/graphql"
	actualCostTTL     = time.Hour
	workersBaseUSD    = 5.0
)

var costAlertsUSD = []float64{15, 30}

func (s *Server) adminSummary(w http.ResponseWriter, r *http.Request, _ string) {
	summary := map[string]any{}
	if err := s.worker.getJSON(r.Context(), "/admin/summary", "", "", &summary); err != nil {
		managedError(w, http.StatusBadGateway, err.Error())
		return
	}
	summary["relay"] = s.relayCounts()
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) relayCounts() map[string]any {
	s.mu.Lock()
	counts := map[string]any{"daemons_connected": len(s.connections), "users_with_daemons": len(s.users), "oauth_grants_active": 0}
	s.mu.Unlock()
	if s.oauth == nil {
		return counts
	}
	active := 0
	err := s.oauth.store.view(func(d *oauthData) error {
		for _, g := range d.Grants.all() {
			if g.Active && !g.Revoked {
				active++
			}
		}
		return nil
	})
	if err != nil {
		counts["oauth_grants_active"] = nil
	} else {
		counts["oauth_grants_active"] = active
	}
	return counts
}

type estimateDay struct {
	Day          string  `json:"day"`
	ModelUSD     float64 `json:"model_usd"`
	ContainerUSD float64 `json:"container_usd"`
	DOUSD        float64 `json:"do_usd"`
	TotalUSD     float64 `json:"total_usd"`
}

// adminCosts is GET /api/v1/admin/costs?days=&refresh=1.
func (s *Server) adminCosts(w http.ResponseWriter, r *http.Request, _ string) {
	now := time.Now().UTC()
	days, err := strconv.Atoi(r.URL.Query().Get("days"))
	if err != nil || days < 1 {
		days = 30
	}
	days = max(min(days, 90), now.Day())
	var usage struct {
		Items []struct {
			Day          string  `json:"day"`
			ModelUSD     float64 `json:"model_usd"`
			ContainerUSD float64 `json:"container_usd"`
		} `json:"items"`
	}
	if err := s.worker.getJSON(r.Context(), "/admin/usage", "days="+strconv.Itoa(days), "", &usage); err != nil {
		managedError(w, http.StatusBadGateway, err.Error())
		return
	}
	byDay := map[string]*estimateDay{}
	for _, item := range usage.Items {
		day := byDay[item.Day]
		if day == nil {
			day = &estimateDay{Day: item.Day}
			byDay[item.Day] = day
		}
		day.ModelUSD += item.ModelUSD
		day.ContainerUSD += item.ContainerUSD
		day.TotalUSD = day.ModelUSD + day.ContainerUSD + day.DOUSD
	}
	estimate := make([]estimateDay, 0, len(byDay))
	month := 0.0
	for _, day := range byDay {
		estimate = append(estimate, *day)
		if day.Day >= monthStart(now) {
			month += day.TotalUSD
		}
	}
	sort.Slice(estimate, func(i, j int) bool { return estimate[i].Day < estimate[j].Day })

	out := map[string]any{
		"estimate": map[string]any{"days": estimate, "month_usd": month, "projected_usd": project(month, now)},
		"actual":   nil,
		"alerts":   costAlertsUSD,
		"as_of":    now.Format(time.RFC3339),
	}
	if actual := s.costs.get(r.Context(), r.URL.Query().Get("refresh") == "1", now); actual != nil {
		out["actual"] = actual
	}
	writeJSON(w, http.StatusOK, out)
}

func monthStart(now time.Time) string { return now.Format("2006-01") + "-01" }

func daysInMonth(now time.Time) int {
	return time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func project(mtd float64, now time.Time) float64 {
	return mtd * float64(daysInMonth(now)) / float64(now.Day())
}

// tier is one billed unit with a monthly included allowance.
type tier struct {
	unit     string
	included float64
	usd      float64 // per unit beyond the allowance
}

// datasetMeter turns one GraphQL dataset's daily rows into billed units.
type datasetMeter struct {
	dataset   string
	selection string
	units     func(row map[string]any, monthDays int) map[string]float64
}

var (
	containerTiers = []tier{{"vcpu_s", 22_500, 2e-5}, {"gib_s", 90_000, 2.5e-6}, {"gb_s", 720_000, 7e-8}, {"egress_gb", 1_000, 0.025}}
	doTiers        = []tier{{"do_requests", 1e6, 0.15e-6}, {"do_gb_s", 400_000, 12.5e-6}, {"do_gb_month", 5, 0.20}}
	workersTiers   = []tier{{"workers_requests", 10e6, 0.30e-6}, {"workers_cpu_ms", 30e6, 0.02e-6}}
	driverTiers    = map[string][]tier{"containers": containerTiers, "do": doTiers, "workers": workersTiers}
)

const (
	neuronsFreePerDay = 10_000
	usdPerNeuron      = 0.011 / 1000
)

// Field names marked unconfirmed come from the metering note, not Cloudflare's
// docs; a wrong one fails only that dataset, which then reports unavailable.
var costMeters = []datasetMeter{
	{"aiInferenceAdaptiveGroups", "sum { totalNeurons }", func(row map[string]any, _ int) map[string]float64 { // unconfirmed
		return map[string]float64{"neurons": num(row, "sum", "totalNeurons")}
	}},
	{"containersUsageAdaptiveGroups", "sum { cpuTimeSec allocatedMemory allocatedDisk txBytes }", func(row map[string]any, _ int) map[string]float64 {
		return map[string]float64{
			"vcpu_s":    num(row, "sum", "cpuTimeSec"),
			"gib_s":     num(row, "sum", "allocatedMemory") / (1 << 30),
			"gb_s":      num(row, "sum", "allocatedDisk") / 1e9,
			"egress_gb": num(row, "sum", "txBytes") / 1e9,
		}
	}},
	{"durableObjectsInvocationsAdaptiveGroups", "sum { requests }", func(row map[string]any, _ int) map[string]float64 {
		return map[string]float64{"do_requests": num(row, "sum", "requests")}
	}},
	{"durableObjectsPeriodicGroups", "sum { activeTime }", func(row map[string]any, _ int) map[string]float64 { // unconfirmed, microseconds
		return map[string]float64{"do_gb_s": num(row, "sum", "activeTime") / 1e6 * 0.125}
	}},
	{"durableObjectsStorageGroups", "max { storedBytes }", func(row map[string]any, monthDays int) map[string]float64 {
		return map[string]float64{"do_gb_month": num(row, "max", "storedBytes") / 1e9 / float64(monthDays)}
	}},
	{"workersInvocationsAdaptive", "sum { requests cpuTimeUs }", func(row map[string]any, _ int) map[string]float64 { // cpuTimeUs unconfirmed
		return map[string]float64{"workers_requests": num(row, "sum", "requests"), "workers_cpu_ms": num(row, "sum", "cpuTimeUs") / 1000}
	}},
}

func num(row map[string]any, group, field string) float64 {
	values, _ := row[group].(map[string]any)
	value, _ := values[field].(float64)
	return value
}

type actualDay struct {
	Day           string             `json:"day"`
	AIUSD         float64            `json:"ai_usd"`
	ContainersUSD float64            `json:"containers_usd"`
	DOUSD         float64            `json:"do_usd"`
	WorkersUSD    float64            `json:"workers_usd"`
	TotalUSD      float64            `json:"total_usd"`
	Units         map[string]float64 `json:"units"`
}

type actualMonth struct {
	BaseUSD       float64 `json:"base_usd"`
	AIUSD         float64 `json:"ai_usd"`
	ContainersUSD float64 `json:"containers_usd"`
	DOUSD         float64 `json:"do_usd"`
	WorkersUSD    float64 `json:"workers_usd"`
	TotalUSD      float64 `json:"total_usd"`
}

// actualCosts is billable spend month to date. Each day carries the overage
// that day pushed past the monthly allowance, so the days sum to the month.
type actualCosts struct {
	Month        actualMonth       `json:"month"`
	ProjectedUSD float64           `json:"projected_usd"`
	Days         []actualDay       `json:"days"`
	Unavailable  map[string]string `json:"unavailable"`
	AsOf         string            `json:"as_of"`
}

type costCache struct {
	mu        sync.Mutex
	accountID string
	token     string
	endpoint  string
	client    *http.Client
	cached    *actualCosts
	fetchedAt time.Time
}

func newCostCache() *costCache {
	return &costCache{endpoint: cfGraphQLEndpoint, client: &http.Client{Timeout: 20 * time.Second}}
}

func (c *costCache) configure(accountID, token string) {
	c.accountID, c.token = accountID, token
}

// get returns nil when unconfigured. The hour-long cache is filled on demand
// by an admin request, never on a timer; a new month invalidates it.
func (c *costCache) get(ctx context.Context, refresh bool, now time.Time) *actualCosts {
	if c.accountID == "" || c.token == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fresh := c.cached != nil && now.Sub(c.fetchedAt) < actualCostTTL && c.fetchedAt.Month() == now.Month()
	if !fresh || refresh {
		c.cached = c.fetch(ctx, now)
		c.fetchedAt = now
	}
	return c.cached
}

func (c *costCache) fetch(ctx context.Context, now time.Time) *actualCosts {
	monthDays := daysInMonth(now)
	units := map[string]map[string]float64{} // day -> unit -> value
	unavailable := map[string]string{}
	for _, meter := range costMeters {
		rows, err := c.query(ctx, meter, now)
		if err != nil {
			unavailable[meter.dataset] = err.Error()
			continue
		}
		for _, row := range rows {
			dims, _ := row["dimensions"].(map[string]any)
			day, _ := dims["date"].(string)
			if day == "" {
				continue
			}
			if units[day] == nil {
				units[day] = map[string]float64{}
			}
			for unit, value := range meter.units(row, monthDays) {
				units[day][unit] += value
			}
		}
	}
	return priceActual(units, unavailable, now)
}

func priceActual(units map[string]map[string]float64, unavailable map[string]string, now time.Time) *actualCosts {
	order := make([]string, 0, len(units))
	for day := range units {
		order = append(order, day)
	}
	sort.Strings(order)
	cumulative := map[string]float64{}
	overage := func(t tier, add float64) float64 {
		before := cumulative[t.unit]
		cumulative[t.unit] = before + add
		return (max(0, before+add-t.included) - max(0, before-t.included)) * t.usd
	}
	out := &actualCosts{Days: []actualDay{}, Unavailable: unavailable, AsOf: now.Format(time.RFC3339)}
	out.Month.BaseUSD = workersBaseUSD
	for _, date := range order {
		u := units[date]
		cost := map[string]float64{}
		for driver, tiers := range driverTiers {
			for _, t := range tiers {
				cost[driver] += overage(t, u[t.unit])
			}
		}
		day := actualDay{Day: date, Units: u, AIUSD: max(0, u["neurons"]-neuronsFreePerDay) * usdPerNeuron,
			ContainersUSD: cost["containers"], DOUSD: cost["do"], WorkersUSD: cost["workers"]}
		day.TotalUSD = day.AIUSD + day.ContainersUSD + day.DOUSD + day.WorkersUSD
		out.Days = append(out.Days, day)
		out.Month.AIUSD += day.AIUSD
		out.Month.ContainersUSD += day.ContainersUSD
		out.Month.DOUSD += day.DOUSD
		out.Month.WorkersUSD += day.WorkersUSD
	}
	m := &out.Month
	m.TotalUSD = m.BaseUSD + m.AIUSD + m.ContainersUSD + m.DOUSD + m.WorkersUSD
	out.ProjectedUSD = m.BaseUSD + project(m.TotalUSD-m.BaseUSD, now)
	return out
}

// query runs one dataset per request so a missing dataset or field degrades
// only its own figures.
func (c *costCache) query(ctx context.Context, meter datasetMeter, now time.Time) ([]map[string]any, error) {
	query := fmt.Sprintf(`query($accountTag: string!, $start: Date!, $end: Date!) { viewer { accounts(filter: {accountTag: $accountTag}) { rows: %s(limit: 100, filter: {date_geq: $start, date_leq: $end}) { dimensions { date } %s } } } }`, meter.dataset, meter.selection)
	body, _ := json.Marshal(map[string]any{"query": query, "variables": map[string]any{
		"accountTag": c.accountID, "start": monthStart(now), "end": now.Format(time.DateOnly),
	}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		Data struct {
			Viewer struct {
				Accounts []struct {
					Rows []map[string]any `json:"rows"`
				} `json:"accounts"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("cloudflare analytics returned %d: %w", resp.StatusCode, err)
	}
	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("cloudflare analytics: %s", result.Errors[0].Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cloudflare analytics returned %d", resp.StatusCode)
	}
	if len(result.Data.Viewer.Accounts) == 0 {
		return nil, fmt.Errorf("cloudflare analytics: account not visible to this token")
	}
	return result.Data.Viewer.Accounts[0].Rows, nil
}
