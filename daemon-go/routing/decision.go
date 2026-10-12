// Package routing records and replays ask-routing decisions.
//
// Every ask attempt writes one Decision into the delivery-trace ledger as the
// route_decision stage of the ask's own trace. A Decision freezes what any
// router could have known at that instant: the request, the sender, and every
// registered peer with its state split into attested fields (observed by the
// daemon) and claimed fields (written by the peer about itself). Outcomes are
// later stages of the same trace. Static replay rebuilds the candidate set from
// the Decision and re-runs any arm offline against the declared target.
package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/repowire/repowire/daemon-go/proto"
)

// SchemaV1 versions the route_decision detail. Bump it on any change a replay
// reader would need to branch on.
const SchemaV1 = "repowire.route_decision.v1"

// Stage is the delivery-trace stage a Decision is recorded under.
const Stage = "route_decision"

// ShadowStage records one shadow arm's result in the same trace. Shadow arms
// run after delivery so they never add latency to an explicit ask.
const ShadowStage = "route_shadow"

// TODO(retention): delivery_traces is never pruned and each route_decision
// can carry up to maxTextBytes of ask text. Fine for one mesh for months; a
// busy mesh needs a retention policy. record_text=false drops the text now.
const (
	maxTextBytes        = 16 << 10
	maxDescriptionBytes = 1 << 10
)

// Surfaces that open an ask.
const (
	SurfaceAsk     = "ask"
	SurfaceAskMany = "ask_many"
	SurfaceQuery   = "query"
)

// Declared-target resolutions.
const (
	ResolutionResolved  = "resolved"
	ResolutionUnknown   = "unknown"
	ResolutionAmbiguous = "ambiguous"
	ResolutionAbsent    = "absent"
)

// Candidate access verdicts, computed with the delivery path's own rules.
const (
	AccessOK             = "ok"
	AccessCircleBoundary = "circle_boundary"
	AccessNotAddressable = "not_addressable"
	// AccessServiceExcluded marks a service- or human-role peer on a
	// targetless decision: reachable, but not a peer a strategy may deliver
	// to unless routing.allow_service_targets is set. Named asks never carry
	// it. Circle and addressability verdicts take precedence.
	AccessServiceExcluded = "service_excluded"
)

// Selection policies, stamped on targetless decisions so rows recorded under
// different policies stay distinguishable.
const (
	PolicyExcludeServiceTargets = "exclude_service_targets"
	PolicyAllowServiceTargets   = "allow_service_targets"
)

// ServiceRole reports whether role marks a peer a strategy must not deliver
// to by default: a service (phone bot, relay) or a human. Role is
// self-declared, so this is operating policy, not a security boundary.
func ServiceRole(role string) bool { return role == "service" || role == "human" }

// ArmExplicit is the passthrough arm: the sender's declared target is delivered.
const ArmExplicit = "explicit"

// Candidate is one peer as a router could see it at decision time.
type Candidate struct {
	PeerID      string `json:"peer_id"`
	DisplayName string `json:"display_name"`

	// Attested: observed by the daemon.
	Circle            string   `json:"circle"`
	Role              string   `json:"role"`
	Backend           string   `json:"backend"`
	Model             string   `json:"model,omitempty"`
	Path              string   `json:"path"`
	Project           string   `json:"project"`
	Machine           string   `json:"machine"`
	Status            string   `json:"status"`
	TurnState         string   `json:"turn_state"`
	LastSeen          *string  `json:"last_seen"`
	LastSeenAgeS      *float64 `json:"last_seen_age_s"`
	Addressable       bool     `json:"addressable"`
	AddressableReason string   `json:"addressable_reason,omitempty"`
	Ephemeral         bool     `json:"ephemeral"`
	Source            string   `json:"source,omitempty"`
	OpenAsksInbound   int      `json:"open_asks_inbound"`
	LastExchangeAt    *string  `json:"last_exchange_with_sender_at"`

	// Claimed: written by the peer about itself.
	Description      string   `json:"description"`
	DescriptionSetAt *string  `json:"description_set_at"`
	DescriptionAgeS  *float64 `json:"description_age_s"`

	// Access is empty on the sender snapshot.
	Access string `json:"access,omitempty"`
}

// Declared is the target the sender named, if any.
type Declared struct {
	Raw        *string `json:"raw"`
	PeerID     *string `json:"peer_id"`
	Resolution string  `json:"resolution"`
}

// Request is the ask as sent.
type Request struct {
	Circle          *string `json:"circle"`
	BypassCircle    bool    `json:"bypass_circle"`
	ReplyTo         *string `json:"reply_to"`
	Text            string  `json:"text"`
	TextSHA256      string  `json:"text_sha256"`
	TextTruncated   bool    `json:"text_truncated"`
	TextRecorded    bool    `json:"text_recorded"`
	HasQuestion     bool    `json:"has_question"`
	AttachmentCount int     `json:"attachment_count"`
}

// Chosen is the peer the deciding arm picked.
type Chosen struct {
	PeerID      string `json:"peer_id"`
	DisplayName string `json:"display_name"`
}

// Score is one candidate's router score.
type Score struct {
	PeerID string  `json:"peer_id"`
	Score  float64 `json:"score"`
}

// RouterResult is what a non-explicit arm returned, or why it fell back.
type RouterResult struct {
	Arm            string             `json:"arm"`
	Model          string             `json:"model,omitempty"`
	TemplateSHA256 string             `json:"template_sha256,omitempty"`
	LatencyMS      int64              `json:"latency_ms"`
	Confidence     *float64           `json:"confidence"`
	Scores         []Score            `json:"scores"`
	Signals        map[string]float64 `json:"signals,omitempty"`
	Seed           *int64             `json:"seed,omitempty"`
	Fallback       bool               `json:"fallback"`
	FallbackReason string             `json:"fallback_reason,omitempty"`
	// HTTPStatus, Provider and ErrorCode attribute a failure. No provider
	// body or other free text is ever stored (see attribution.go).
	HTTPStatus int    `json:"http_status,omitempty"`
	Provider   string `json:"provider,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	// Error is never written. Rows recorded before #420 hold raw provider
	// text here; read them only through Attribution, never export it.
	Error string `json:"error,omitempty"`
}

// Decision is the route_decision detail.
type Decision struct {
	Schema    string        `json:"schema"`
	Surface   string        `json:"surface"`
	ParentID  *string       `json:"parent_id"`
	DecidedAt string        `json:"decided_at"`
	Mode      string        `json:"mode"`
	Arm       string        `json:"arm"`
	Sender    *Candidate    `json:"sender"`
	SenderRaw string        `json:"sender_raw"`
	Declared  Declared      `json:"declared"`
	Request   Request       `json:"request"`
	Chosen    *Chosen       `json:"chosen"`
	Router    *RouterResult `json:"router"`
	// ShadowPlanned lists the shadow arms sampled to run for this ask; each
	// writes a route_shadow row. An arm absent here did not run, so replay
	// knows which live results are missing by design.
	ShadowPlanned     []string       `json:"shadow_planned"`
	ShadowSampleEvery map[string]int `json:"shadow_sample_every,omitempty"`
	// ShadowSkipped records planned arms that will not run (the shadow pool
	// was full when the ask was planned), with their configured identity, so
	// no extra row has to be written on the request path.
	ShadowSkipped []RouterResult `json:"shadow_skipped,omitempty"`
	// SelectionPolicy is set on targetless decisions only: which peers the
	// strategy was allowed to deliver to.
	SelectionPolicy string      `json:"selection_policy,omitempty"`
	RegistrySize    int         `json:"registry_size"`
	Candidates      []Candidate `json:"candidates"`
}

// Detail renders the Decision as a trace detail map.
func (d Decision) Detail() map[string]any { return toMap(d) }

// Detail renders a shadow result as a route_shadow trace detail map.
func (r RouterResult) Detail() map[string]any { return toMap(r) }

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}

// ParseDecision reads a route_decision detail back.
func ParseDecision(detail map[string]any) (Decision, error) {
	var d Decision
	err := remap(detail, &d)
	return d, err
}

// PeerStats is the AskTracker's view of a candidate relative to the sender.
type PeerStats struct {
	OpenInbound  int
	LastExchange *time.Time
}

// Snapshot freezes one peer. descSetAt is when the registry last saw the peer
// set its description (nil when unknown, e.g. since a daemon restart).
func Snapshot(p *proto.Peer, now time.Time, descSetAt *time.Time, stats PeerStats) Candidate {
	c := Candidate{
		PeerID:            string(p.PeerID),
		DisplayName:       string(p.DisplayName),
		Circle:            p.Circle,
		Role:              string(p.Role),
		Backend:           string(p.Backend),
		Path:              p.Path,
		Project:           project(p),
		Machine:           p.Machine,
		Status:            string(p.Status),
		TurnState:         string(p.TurnState),
		Addressable:       p.Addressable,
		AddressableReason: p.AddressableReason,
		Ephemeral:         p.Ephemeral,
		Source:            string(p.Source),
		OpenAsksInbound:   stats.OpenInbound,
		Description:       clip(p.Description, maxDescriptionBytes),
	}
	if p.Model != nil {
		c.Model = *p.Model
	}
	c.LastSeen, c.LastSeenAgeS = stamp(p.LastSeen, now)
	c.LastExchangeAt, _ = stamp(stats.LastExchange, now)
	c.DescriptionSetAt, c.DescriptionAgeS = stamp(descSetAt, now)
	return c
}

// NewRequest captures the ask body, bounding the stored text.
func NewRequest(text string, circle, replyTo *string, bypass, hasQuestion bool, attachments int) Request {
	sum := sha256.Sum256([]byte(text))
	stored := clip(text, maxTextBytes)
	return Request{
		Circle: circle, BypassCircle: bypass, ReplyTo: replyTo,
		Text: stored, TextSHA256: hex.EncodeToString(sum[:]), TextTruncated: stored != text, TextRecorded: true,
		HasQuestion: hasQuestion, AttachmentCount: attachments,
	}
}

// project is the hook-reported project name, else the path's base name.
func project(p *proto.Peer) string {
	if v, _ := p.Metadata["project"].(string); v != "" {
		return v
	}
	for i := len(p.Path) - 1; i >= 0; i-- {
		if p.Path[i] == '/' {
			return p.Path[i+1:]
		}
	}
	return p.Path
}

func stamp(t *time.Time, now time.Time) (*string, *float64) {
	if t == nil || t.IsZero() {
		return nil, nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	age := now.Sub(*t).Seconds()
	return &s, &age
}

// clip bounds s to max bytes without splitting a rune.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
