package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/hooks"
	"gopkg.in/yaml.v3"
)

var Version = "0.18.0"

func Run(argv []string) int {
	if len(argv) == 0 {
		return help()
	}
	for _, arg := range argv[1:] {
		if arg == "help" || arg == "--help" || arg == "-h" {
			return commandHelp(argv[0])
		}
	}
	switch argv[0] {
	case "help", "--help", "-h":
		return help()
	case "version", "--version":
		fmt.Println(Version)
		return 0
	case "setup":
		return runSetup(argv[1:])
	case "status":
		return runStatus()
	case "doctor":
		return runDoctor()
	case "link":
		return runLink(argv[1:])
	case "peer":
		return runPeer(argv[1:])
	case "jobs":
		return runJobs(argv[1:])
	case "schedule":
		return runSchedule(argv[1:])
	case "session":
		return runSession(argv[1:])
	case "trace":
		return runTrace(argv[1:])
	case "share":
		return runShare(argv[1:])
	case "relay":
		return runRelay(argv[1:])
	case "telegram", "slack":
		return runBot(argv[0], argv[1:])
	case "agents":
		return runAgents(argv[1:])
	case "service":
		return runService(argv[1:])
	case "daemon":
		return runDaemonCommand(argv[1:])
	case "config":
		return runConfig(argv[1:])
	case "hooks":
		return runRuntimeInstall("claude-code", argv[1:])
	case "claude":
		return runRuntimeInstall("claude-code", argv[1:])
	case "codex", "gemini", "opencode", "antigravity", "pi":
		return runRuntimeInstall(argv[0], argv[1:])
	case "memory":
		return runMemory(argv[1:])
	case "orchestrator":
		return runOrchestrator(argv[1:])
	case "build-ui":
		return runBuildUI()
	case "update":
		return runUpdate()
	case "uninstall":
		return runUninstall(argv[1:])
	default:
		return usage("<setup|serve|status|doctor|link|peer|jobs|schedule|session|trace|share|relay|telegram|slack|agents|service|config|memory|orchestrator>")
	}
}

func commandHelp(command string) int {
	fmt.Printf("Usage: repowire %s [options]\n\nSee docs/reference/cli.md for command details.\n", command)
	return 0
}

func help() int {
	fmt.Println("Repowire - mesh network for AI coding agents\n\nCommands: setup, serve, status, doctor, link, peer, jobs, schedule, session, trace, share, relay, telegram, slack, agents, service, config, memory, orchestrator")
	return 0
}

func runStatus() int {
	fmt.Println("repowire:", Version)
	for _, name := range []string{"claude-code", "codex", "gemini", "antigravity", "opencode", "pi"} {
		if runtimeAvailable(name) || runtimeIntegrated(name) {
			fmt.Printf("%s: runtime=%s integration=%s\n", name,
				map[bool]string{true: "detected", false: "missing"}[runtimeAvailable(name)],
				map[bool]string{true: "installed", false: "missing"}[runtimeIntegrated(name)])
		}
	}
	c, err := newClient()
	if err != nil {
		return fatal(err)
	}
	health, err := c.request(http.MethodGet, "/health", nil)
	if err != nil {
		return fatal(err)
	}
	fmt.Printf("daemon: ok (%s)\npeers: %v\nschema: %v\n", c.base, health["peers"], health["schema_version"])
	if relay, ok := health["relay"].(map[string]any); ok {
		fmt.Printf("relay: %v\n", first(stringValue(relay, "state"), stringValue(relay, "status")))
	}
	if updatesEnabled() {
		if latest, err := latestVersion(); err == nil && versionGreater(latest, Version) {
			fmt.Printf("update: %s available (run repowire update)\n", latest)
		}
	}
	return 0
}
func runDoctor() int {
	code := runStatus()
	for _, binary := range []string{"tmux", "git"} {
		if _, err := execLookPath(binary); err != nil {
			fmt.Printf("warn: %s not found\n", binary)
		}
	}
	return code
}

func updatesEnabled() bool {
	raw, err := os.ReadFile(config.Path())
	if err != nil {
		return false
	}
	var data map[string]any
	if yaml.Unmarshal(raw, &data) != nil {
		return false
	}
	updates, _ := data["updates"].(map[string]any)
	enabled, _ := updates["check_enabled"].(bool)
	return enabled
}

func latestVersion() (string, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	request, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/prassanna-ravishankar/repowire/releases/latest", nil)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "repowire/"+Version)
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitHub returned %s", response.Status)
	}
	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return "", err
	}
	return strings.TrimPrefix(payload.TagName, "v"), nil
}

func versionGreater(candidate, current string) bool {
	left, right := strings.Split(strings.TrimPrefix(candidate, "v"), "."), strings.Split(strings.TrimPrefix(current, "v"), ".")
	for index := 0; index < 3; index++ {
		a, b := versionPart(left, index), versionPart(right, index)
		if a != b {
			return a > b
		}
	}
	return false
}

func versionPart(parts []string, index int) int {
	if index >= len(parts) {
		return 0
	}
	fields := strings.FieldsFunc(parts[index], func(r rune) bool { return r < '0' || r > '9' })
	if len(fields) == 0 {
		return 0
	}
	value, _ := strconv.Atoi(fields[0])
	return value
}

func runLink(argv []string) int {
	a := parse(argv)
	pane := a.string("pane", os.Getenv("TMUX_PANE"))
	backend := a.string("backend", "")
	if pane == "" || backend == "" {
		return usage("link --pane %42 --backend BACKEND [--name NAME] [--circle C]")
	}
	body := map[string]any{"backend": backend}
	copyFlags(body, a, "name", "circle", "cwd")
	c, err := newClient()
	if err != nil {
		return fatal(err)
	}
	result, err := c.request(http.MethodPost, "/panes/"+url.PathEscape(pane)+"/link", body)
	if err != nil {
		return fatal(err)
	}
	printJSON(result)
	if linked, _ := result["linked"].(bool); !linked {
		return 1
	}
	return 0
}

func runPeer(argv []string) int {
	if len(argv) == 0 {
		return usage("peer <list|describe|new|register|unregister|restart|doctor|rehook|ask|prune|whoami|asks|deliveries|ack|claim-role>")
	}
	a := parse(argv[1:], "show-offline", "dry-run", "json", "fix", "apply", "register", "force")
	c, err := newClient()
	if err != nil {
		return fatal(err)
	}
	switch argv[0] {
	case "list":
		q := url.Values{}
		if !a.bool("show-offline") {
			q.Set("status", "online")
		}
		result, err := c.request(http.MethodGet, pathQuery("/peers", q), nil)
		if err != nil {
			return fatal(err)
		}
		printPeers(result)
		return 0
	case "describe":
		if len(a.pos) < 1 {
			return usage("peer describe NAME_OR_ID [--circle C]")
		}
		q := url.Values{}
		addQuery(q, "circle", a.string("circle", ""))
		result, err := c.request(http.MethodGet, pathQuery("/peers/"+url.PathEscape(a.pos[0]), q), nil)
		if err != nil {
			return fatal(err)
		}
		printJSON(result)
		return 0
	case "new":
		if len(a.pos) < 1 {
			return usage("peer new PATH --backend BACKEND")
		}
		currentCircle := currentTmuxCircle()
		circle := first(a.string("circle", ""), currentCircle)
		if circle == "" {
			return usage("peer new PATH --circle CIRCLE [--backend BACKEND]")
		}
		body := map[string]any{"path": abs(a.pos[0]), "backend": a.string("backend", "claude-code"), "circle": circle}
		if pane := os.Getenv("TMUX_PANE"); pane != "" && circle == currentCircle {
			body["source_pane"] = pane
		}
		for _, key := range []string{"profile", "command", "message"} {
			if value := a.string(key, ""); value != "" {
				body[key] = value
			}
		}
		result, err := c.request(http.MethodPost, "/spawn", body)
		if err != nil {
			return fatal(err)
		}
		printJSON(result)
		return 0
	case "register":
		path := a.string("path", mustGetwd())
		circle := first(a.string("circle", ""), currentTmuxCircle())
		if circle == "" {
			return usage("peer register requires --circle CIRCLE outside tmux")
		}
		body := map[string]any{"name": a.string("name", filepath.Base(path)), "path": abs(path), "backend": a.string("backend", "claude-code"), "circle": circle}
		result, err := c.request(http.MethodPost, "/peers", body)
		if err != nil {
			return fatal(err)
		}
		printJSON(result)
		return 0
	case "unregister":
		if len(a.pos) < 1 {
			return usage("peer unregister NAME_OR_ID")
		}
		_, err := c.request(http.MethodDelete, "/peers/"+url.PathEscape(a.pos[0]), nil)
		if err != nil {
			return fatal(err)
		}
		fmt.Println("unregistered", a.pos[0])
		return 0
	case "restart":
		if len(a.pos) < 1 {
			return usage("peer restart NAME_OR_ID [--dry-run] [-m MESSAGE]")
		}
		body := map[string]any{"dry_run": a.bool("dry-run")}
		if value := a.string("message", ""); value != "" {
			body["message"] = value
		}
		if value := a.string("circle", ""); value != "" {
			body["circle"] = value
		}
		result, err := c.request(http.MethodPost, "/peers/"+url.PathEscape(a.pos[0])+"/restart", body)
		if err != nil {
			return fatal(err)
		}
		printJSON(result)
		return 0
	case "doctor":
		if len(a.pos) < 1 {
			return usage("peer doctor NAME_OR_ID")
		}
		q := url.Values{}
		addQuery(q, "circle", a.string("circle", ""))
		result, err := c.request(http.MethodGet, pathQuery("/peers/"+url.PathEscape(a.pos[0])+"/doctor", q), nil)
		if err != nil {
			return fatal(err)
		}
		printJSON(result)
		if a.bool("fix") && hasInboundContradiction(result) {
			body := map[string]any{"apply": true}
			if circle := a.string("circle", ""); circle != "" {
				body["circle"] = circle
			}
			fixed, fixErr := c.request(http.MethodPost, "/peers/"+url.PathEscape(a.pos[0])+"/rehook", body)
			if fixErr != nil {
				return fatal(fixErr)
			}
			fmt.Println("rehook repair:")
			printJSON(fixed)
		}
		return contradictionExit(result)
	case "rehook":
		if len(a.pos) < 1 {
			return usage("peer rehook NAME_OR_ID [--apply]")
		}
		body := map[string]any{"apply": a.bool("apply")}
		if value := a.string("circle", ""); value != "" {
			body["circle"] = value
		}
		result, err := c.request(http.MethodPost, "/peers/"+url.PathEscape(a.pos[0])+"/rehook", body)
		if err != nil {
			return fatal(err)
		}
		printJSON(result)
		return 0
	case "ask":
		if len(a.pos) < 2 {
			return usage("peer ask NAME QUERY")
		}
		from := hooks.MCPIdentity()
		body := map[string]any{"from_peer": from, "to_peer": a.pos[0], "text": strings.Join(a.pos[1:], " ")}
		if value := a.string("circle", ""); value != "" {
			body["circle"] = value
		}
		result, err := c.request(http.MethodPost, "/ask", body)
		if err != nil {
			return fatal(err)
		}
		cid := stringValue(result, "correlation_id")
		wait, err := c.request(http.MethodPost, "/asks/"+url.PathEscape(cid)+"/wait", map[string]any{"peer_id": from, "timeout_seconds": a.integer("timeout", 60)})
		if err != nil {
			return fatal(err)
		}
		fmt.Println(first(stringValue(wait, "reply"), stringValue(wait, "message"), jsonText(wait)))
		return 0
	case "prune":
		result, err := c.request(http.MethodGet, "/peers", nil)
		if err != nil {
			return fatal(err)
		}
		offline := []map[string]any{}
		for _, raw := range anySlice(result["peers"]) {
			peer, _ := raw.(map[string]any)
			if stringValue(peer, "status") == "offline" {
				offline = append(offline, peer)
			}
		}
		if len(offline) == 0 {
			fmt.Println("no offline peers to prune")
			return 0
		}
		for _, peer := range offline {
			fmt.Println("offline:", first(stringValue(peer, "display_name"), stringValue(peer, "peer_id")))
		}
		if a.bool("dry-run") {
			fmt.Printf("%d offline peer(s) would be pruned\n", len(offline))
			return 0
		}
		if !a.bool("force") {
			fmt.Fprintf(os.Stderr, "Remove %d offline peer(s)? [y/N] ", len(offline))
			answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			answer = strings.ToLower(strings.TrimSpace(answer))
			if answer != "y" && answer != "yes" {
				fmt.Println("cancelled")
				return 0
			}
		}
		removed := 0
		for _, peer := range offline {
			if _, err := c.request(http.MethodDelete, "/peers/"+url.PathEscape(stringValue(peer, "peer_id")), nil); err == nil {
				removed++
			}
		}
		fmt.Printf("pruned %d peer(s)\n", removed)
		return 0
	case "whoami":
		if a.bool("register") {
			backend := a.string("backend", "")
			if backend == "" {
				return usage("peer whoami --register --backend BACKEND")
			}
			path := abs(a.string("path", mustGetwd()))
			circle := first(a.string("circle", ""), currentTmuxCircle())
			if circle == "" {
				return usage("peer whoami --register --backend BACKEND --circle CIRCLE")
			}
			body := map[string]any{"name": a.string("name", filepath.Base(path)), "path": path, "backend": backend, "circle": circle, "metadata": map[string]any{"repowire_cli_fallback": true}}
			if pane := os.Getenv("TMUX_PANE"); pane != "" {
				body["pane_id"] = pane
			}
			result, err := c.request(http.MethodPost, "/peers", body)
			if err != nil {
				return fatal(err)
			}
			printJSON(result)
			return 0
		}
		id := first(a.string("peer-id", ""), a.string("name", ""))
		lookup := ""
		if pane := os.Getenv("TMUX_PANE"); pane != "" && id == "" {
			lookup = "/peers/by-pane/" + url.PathEscape(pane)
		}
		if lookup == "" {
			if id == "" {
				return fatal(fmt.Errorf("no registered peer for this pane; pass --name or --register"))
			}
			lookup = "/peers/" + url.PathEscape(id)
		}
		result, err := c.request(http.MethodGet, lookup, nil)
		if err != nil {
			return fatal(err)
		}
		printJSON(result)
		return 0
	case "asks":
		return peerPoll(c, a, "/asks/pending", "asks")
	case "deliveries":
		return peerPoll(c, a, "/deliveries/pending", "deliveries")
	case "ack":
		if len(a.pos) < 1 {
			return usage("peer ack CORR_ID [-m MESSAGE]")
		}
		body := map[string]any{"correlation_id": a.pos[0], "from_peer": first(a.string("from-peer", ""), hooks.MCPIdentity())}
		if msg := a.string("message", ""); msg != "" {
			body["message"] = msg
		}
		_, err := c.request(http.MethodPost, "/ack", body)
		if err != nil {
			return fatal(err)
		}
		fmt.Println("acked #" + a.pos[0])
		return 0
	case "claim-role":
		if len(a.pos) < 1 || a.pos[0] != "orchestrator" {
			return usage("peer claim-role orchestrator")
		}
		body := map[string]any{"peer_name": first(a.string("peer", ""), hooks.MCPIdentity()), "role": "orchestrator", "force": a.bool("force")}
		if value := a.string("circle", ""); value != "" {
			body["circle"] = value
		}
		result, err := c.request(http.MethodPost, "/peers/claim-role", body)
		if err != nil {
			return fatal(err)
		}
		printJSON(result)
		return 0
	default:
		return usage("peer <list|describe|new|register|unregister|restart|doctor|rehook|ask|prune|whoami|asks|deliveries|ack|claim-role>")
	}
}

func peerPoll(c *client, a args, path, key string) int {
	q := url.Values{}
	for _, name := range []string{"peer-id", "pane-id", "peer", "direction"} {
		addQuery(q, strings.ReplaceAll(name, "-", "_"), a.string(name, ""))
	}
	if q.Get("peer_id") == "" && q.Get("pane_id") == "" && q.Get("peer") == "" {
		q.Set("peer_id", hooks.MCPIdentity())
	}
	result, err := c.request(http.MethodGet, pathQuery(path, q), nil)
	if err != nil {
		return fatal(err)
	}
	printJSON(result[key])
	return 0
}

func runJobs(argv []string) int {
	if len(argv) == 0 {
		return usage("jobs <create|list|show|update|run|retry|cancel|result>")
	}
	a := parse(argv[1:], "json")
	c, err := newClient()
	if err != nil {
		return fatal(err)
	}
	method, path, body := http.MethodGet, "", any(nil)
	switch argv[0] {
	case "create":
		if len(a.pos) < 1 {
			return usage("jobs create TITLE")
		}
		method, path = http.MethodPost, "/jobs"
		m := map[string]any{"title": strings.Join(a.pos, " "), "kind": a.string("kind", "general"), "created_by_peer_id": hooks.MCPIdentity(), "visibility": a.string("visibility", "circle")}
		copyFlags(m, a, "prompt", "prompt-file", "path", "backend", "profile", "due-at", "cron", "process-scope", "continuity", "result-surface", "circle")
		copyFlagAs(m, a, "assigned-peer", "assigned_peer_id")
		copyFlagAs(m, a, "owner", "owner_peer_id")
		body = m
	case "list":
		q := url.Values{}
		addQuery(q, "state", a.string("state", ""))
		addQuery(q, "owner_peer_id", a.string("owner", ""))
		addQuery(q, "created_by_peer_id", a.string("created-by", ""))
		addQuery(q, "repowire_session_id", a.string("session", ""))
		addQuery(q, "circle", a.string("circle", ""))
		path = pathQuery("/jobs", q)
	case "show":
		if len(a.pos) < 1 {
			return usage("jobs show JOB_ID")
		}
		path = "/jobs/" + url.PathEscape(a.pos[0]) + "/status"
	case "update":
		if len(a.pos) < 1 {
			return usage("jobs update JOB_ID --state STATE")
		}
		method, path = http.MethodPatch, "/jobs/"+url.PathEscape(a.pos[0])
		m := map[string]any{"state": a.string("state", "")}
		copyFlags(m, a, "attempt-id", "reason", "phase", "note", "result-summary")
		body = m
	case "run", "retry":
		if len(a.pos) < 1 {
			return usage("jobs " + argv[0] + " JOB_ID")
		}
		method, path = http.MethodPost, "/jobs/"+url.PathEscape(a.pos[0])+"/"+argv[0]
		body = map[string]any{}
	case "cancel":
		if len(a.pos) < 1 {
			return usage("jobs cancel JOB_ID")
		}
		method, path = http.MethodPost, "/jobs/"+url.PathEscape(a.pos[0])+"/cancel"
		body = map[string]any{"requested_by_peer_id": first(a.string("requested-by", ""), hooks.MCPIdentity()), "reason": a.string("reason", "cancel_requested")}
	case "result":
		if len(a.pos) < 1 {
			return usage("jobs result JOB_ID")
		}
		path = "/jobs/" + url.PathEscape(a.pos[0]) + "/result"
	default:
		return usage("jobs <create|list|show|update|run|retry|cancel|result>")
	}
	result, err := c.request(method, path, body)
	if err != nil {
		return fatal(err)
	}
	printJSON(result)
	return 0
}

func runSchedule(argv []string) int {
	if len(argv) == 0 {
		return usage("schedule <self|create|list|delete>")
	}
	a := parse(argv[1:], "cron")
	c, err := newClient()
	if err != nil {
		return fatal(err)
	}
	switch argv[0] {
	case "self", "create":
		offset := 0
		to := hooks.MCPIdentity()
		if argv[0] == "create" {
			if len(a.pos) < 3 {
				return usage("schedule create TO WHEN TEXT --from-peer FROM")
			}
			to = a.pos[0]
			offset = 1
		} else if len(a.pos) < 2 {
			return usage("schedule self WHEN TEXT")
		}
		when, text := a.pos[offset], strings.Join(a.pos[offset+1:], " ")
		body := map[string]any{"from_peer": first(a.string("from-peer", ""), hooks.MCPIdentity()), "to_peer": to, "text": text, "kind": a.string("kind", "notify")}
		if a.bool("cron") {
			body["cron"] = when
		} else {
			fire, err := parseWhen(when)
			if err != nil {
				return fatal(err)
			}
			body["fire_at"] = fire
		}
		if circle := a.string("circle", ""); circle != "" {
			body["circle"] = circle
		}
		result, err := c.request(http.MethodPost, "/schedules", body)
		if err != nil {
			return fatal(err)
		}
		printJSON(result)
		return 0
	case "list":
		q := url.Values{}
		addQuery(q, "from_peer", a.string("from-peer", ""))
		result, err := c.request(http.MethodGet, pathQuery("/schedules", q), nil)
		if err != nil {
			return fatal(err)
		}
		printJSON(result)
		return 0
	case "delete":
		if len(a.pos) < 1 {
			return usage("schedule delete ID")
		}
		_, err := c.request(http.MethodDelete, "/schedules/"+url.PathEscape(a.pos[0]), nil)
		if err != nil {
			return fatal(err)
		}
		fmt.Println("deleted schedule", a.pos[0])
		return 0
	default:
		return usage("schedule <self|create|list|delete>")
	}
}

func runSession(argv []string) int {
	if len(argv) < 2 || argv[0] != "resume" {
		return usage("session resume SESSION_ID [--dry-run]")
	}
	a := parse(argv[2:], "dry-run", "json")
	c, err := newClient()
	if err != nil {
		return fatal(err)
	}
	body := map[string]any{"repowire_session_id": argv[1], "dry_run": a.bool("dry-run")}
	copyFlags(body, a, "profile", "message")
	result, err := c.request(http.MethodPost, "/sessions/resume", body)
	if err != nil {
		return fatal(err)
	}
	printJSON(result)
	return 0
}
func runTrace(argv []string) int {
	if len(argv) < 1 {
		return usage("trace TRACE_ID")
	}
	c, err := newClient()
	if err != nil {
		return fatal(err)
	}
	result, err := c.request(http.MethodGet, "/traces/"+url.PathEscape(argv[0]), nil)
	if err != nil {
		return fatal(err)
	}
	printJSON(result)
	for _, raw := range anySlice(result["stages"]) {
		stage, _ := raw.(map[string]any)
		if stringValue(stage, "status") == "fail" {
			return 1
		}
	}
	return 0
}
func runShare(argv []string) int {
	a := parse(argv, "rw", "list")
	c, err := newClient()
	if err != nil {
		return fatal(err)
	}
	if a.bool("list") {
		result, err := c.request(http.MethodGet, "/shares", nil)
		if err != nil {
			return fatal(err)
		}
		printJSON(result)
		return 0
	}
	if id := a.string("revoke", ""); id != "" {
		_, err := c.request(http.MethodDelete, "/shares/"+url.PathEscape(id), nil)
		if err != nil {
			return fatal(err)
		}
		fmt.Println("revoked share", id)
		return 0
	}
	if len(a.pos) < 1 {
		return usage("share PEER [--rw] [--ttl SECS]")
	}
	body := map[string]any{"peer_name": a.pos[0], "permissions": map[bool]string{true: "rw", false: "ro"}[a.bool("rw")]}
	if ttl := a.integer("ttl", 0); ttl > 0 {
		body["ttl_secs"] = ttl
	}
	result, err := c.request(http.MethodPost, "/shares", body)
	if err != nil {
		return fatal(err)
	}
	printJSON(result)
	return 0
}

func printPeers(result map[string]any) {
	fmt.Println("peer_id\tname\tproject\tcircle\trole\tstatus\tpath\tbackend\tturn_state\tmodel")
	for _, raw := range anySlice(result["peers"]) {
		p, _ := raw.(map[string]any)
		project := ""
		if meta, ok := p["metadata"].(map[string]any); ok {
			project = stringValue(meta, "project")
		}
		fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", stringValue(p, "peer_id"), first(stringValue(p, "display_name"), stringValue(p, "name")), project, stringValue(p, "circle"), stringValue(p, "role"), stringValue(p, "status"), stringValue(p, "path"), stringValue(p, "backend"), stringValue(p, "turn_state"), stringValue(p, "model"))
	}
}
func contradictionExit(result map[string]any) int {
	for _, raw := range anySlice(result["contradictions"]) {
		item, _ := raw.(map[string]any)
		if stringValue(item, "severity") == "error" {
			return 1
		}
	}
	return 0
}

func hasInboundContradiction(result map[string]any) bool {
	for _, raw := range anySlice(result["contradictions"]) {
		item, _ := raw.(map[string]any)
		switch stringValue(item, "code") {
		case "ONLINE_BUT_NO_WS", "PANE_MISSING", "HOOK_PEERID_MISMATCH":
			return true
		}
	}
	return false
}
func copyFlags(body map[string]any, a args, names ...string) {
	for _, name := range names {
		if value := a.string(name, ""); value != "" {
			body[strings.ReplaceAll(name, "-", "_")] = value
		}
	}
}
func copyFlagAs(body map[string]any, a args, flag, key string) {
	if value := a.string(flag, ""); value != "" {
		body[key] = value
	}
}

func runDaemonCommand(argv []string) int {
	if len(argv) == 0 {
		return usage("daemon <start|stop|status>")
	}
	switch argv[0] {
	case "start":
		if parse(argv[1:], "foreground").bool("foreground") {
			return runExternal(executable(), "serve")
		}
		if err := installService(); err != nil {
			return fatal(err)
		}
		fmt.Println("daemon started")
		return 0
	case "stop":
		if err := stopService(); err != nil {
			return fatal(err)
		}
		fmt.Println("daemon stopped")
		return 0
	case "status":
		return serviceStatus()
	default:
		return usage("daemon <start|stop|status>")
	}
}

func runConfig(argv []string) int {
	if len(argv) == 0 {
		return usage("config <show|path|get KEY>")
	}
	path := config.Path()
	switch argv[0] {
	case "path":
		fmt.Println(path)
		return 0
	case "show":
		raw, err := os.ReadFile(path)
		if err != nil {
			return fatal(err)
		}
		fmt.Print(string(raw))
		return 0
	case "get":
		if len(argv) < 2 {
			return usage("config get KEY")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fatal(err)
		}
		var value any
		if err := yaml.Unmarshal(raw, &value); err != nil {
			return fatal(err)
		}
		for _, key := range strings.Split(argv[1], ".") {
			current, ok := value.(map[string]any)
			if !ok {
				return fatal(fmt.Errorf("config key not found: %s", argv[1]))
			}
			value, ok = current[key]
			if !ok {
				return fatal(fmt.Errorf("config key not found: %s", argv[1]))
			}
		}
		printJSON(value)
		return 0
	default:
		return usage("config <show|path|get KEY>")
	}
}
func abs(path string) string {
	value, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return value
}
func mustGetwd() string         { cwd, _ := os.Getwd(); return cwd }
func jsonText(value any) string { return fmt.Sprint(value) }

var _ = time.Now
