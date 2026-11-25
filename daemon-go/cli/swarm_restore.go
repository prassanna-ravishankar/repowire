package cli

// swarm_restore.go is `repowire swarm restore` (#505 part 2b): the CLI's half
// of a restore. The daemon plans and verifies (POST /swarm/plan, /placed,
// /verify); the CLI owns tmux, because a tmux server inherits the
// environment of the client that starts it, and only the CLI runs in the
// user's own.
//
// Guards, since this creates panes and starts agents:
//   - a dry run unless --apply;
//   - every tmux call names its server with -S, never the environment, and
//     the default server (the one a bare tmux reaches, where the user's
//     agents run) is refused unless --allow-default-server;
//   - create-only: if any session the plan names exists on that server,
//     nothing is created; no pane is ever killed, respawned or typed into,
//     since agent panes are created running their command.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// restorePlan mirrors hub.SwarmPlan.
type restorePlan struct {
	Schema int           `json:"schema"`
	PlanID string        `json:"plan_id"`
	Panes  []restorePane `json:"panes"`
}

type restorePane struct {
	Session         string `json:"session"`
	WindowIndex     int    `json:"window_index"`
	WindowName      string `json:"window_name"`
	WindowLayout    string `json:"window_layout"`
	PaneIndex       int    `json:"pane_index"`
	Action          string `json:"action"`
	CWD             string `json:"cwd"`
	PeerID          string `json:"peer_id,omitempty"`
	Backend         string `json:"backend,omitempty"`
	Command         string `json:"command,omitempty"`
	AwaitsFirstTurn bool   `json:"awaits_first_turn,omitempty"`
	Skip            string `json:"skip,omitempty"`
}

type placedPane struct {
	PlanID      string `json:"plan_id"`
	PaneID      string `json:"pane_id"`
	PeerID      string `json:"peer_id"`
	TmuxSession string `json:"tmux_session"`
}

type verifiedAgent struct {
	placedPane
	Outcome string `json:"outcome"`
	Got     string `json:"got,omitempty"`
}

// restoreOptions are restore's knobs; tests shorten the waits.
type restoreOptions struct {
	apply, allowDefault bool
	socket              string
	register            time.Duration // how long a launch waits for the previous one at its path
	poll                time.Duration
}

func swarmRestore(c *client, a args) int {
	if len(a.pos) < 1 {
		return usage("swarm restore FILE [--apply] [--socket PATH | --allow-default-server]")
	}
	opts := restoreOptions{apply: a.bool("apply"), allowDefault: a.bool("allow-default-server"), socket: a.string("socket", ""),
		register: 30 * time.Second, poll: time.Second}
	return runRestore(c, a.pos[0], opts)
}

func runRestore(c *client, file string, opts restoreOptions) int {
	raw, err := os.ReadFile(file)
	if err != nil {
		return fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fatal(fmt.Errorf("%s is not a swarm snapshot: %w", file, err))
	}
	result, err := c.request(http.MethodPost, "/swarm/plan", manifest)
	if err != nil {
		return fatal(err)
	}
	var plan restorePlan
	if err := remarshal(result, &plan); err != nil {
		return fatal(err)
	}
	printPlan(plan)
	if !opts.apply {
		fmt.Println("dry run: nothing was created. Re-run with --apply (and --socket or --allow-default-server) to restore.")
		return 0
	}
	socket, err := restoreSocket(opts)
	if err != nil {
		return fatal(err)
	}
	t := tmuxAt{socket: socket}
	if err := t.refuseExisting(plan); err != nil {
		return fatal(err)
	}
	placed, err := applyPlan(c, t, plan, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "repowire: restore stopped:", err)
	}
	report := verifyPlaced(c, placed, opts)
	for _, v := range report {
		line := fmt.Sprintf("%-18s %s in %s", v.Outcome, v.PeerID, v.PaneID)
		if v.Got != "" {
			line += " (registered as " + v.Got + ")"
		}
		fmt.Println(line)
	}
	if err != nil {
		return 1
	}
	return 0
}

func printPlan(plan restorePlan) {
	for _, p := range plan.Panes {
		what := "shell " + p.CWD
		switch {
		case p.Action == "launch":
			what = "launch " + p.PeerID + " in " + p.CWD
			if p.AwaitsFirstTurn {
				what += " (awaits its first turn)"
			}
		case p.Skip != "":
			what += "  [" + p.PeerID + " not relaunched: " + p.Skip + "]"
		}
		fmt.Printf("%s:%d.%d  %s\n", p.Session, p.WindowIndex, p.PaneIndex, what)
	}
}

// restoreSocket is the server a restore creates on: --socket, else the
// default server, which needs --allow-default-server.
func restoreSocket(opts restoreOptions) (string, error) {
	defaults := defaultTmuxSockets()
	if opts.socket != "" {
		abs, err := filepath.Abs(opts.socket)
		if err != nil {
			return "", err
		}
		if defaults[realPath(abs)] && !opts.allowDefault {
			return "", fmt.Errorf("%s is the default tmux server, where your agents run; pass --allow-default-server to restore into it", abs)
		}
		return abs, nil
	}
	if !opts.allowDefault {
		return "", errors.New("restore creates sessions on a tmux server: name one with --socket PATH, or pass --allow-default-server to use the default server (create-only: existing sessions are never touched)")
	}
	return defaultTmuxSocket(), nil
}

// defaultTmuxSocket is the socket a bare tmux would reach: the one in $TMUX,
// else default under $TMUX_TMPDIR or /tmp.
func defaultTmuxSocket() string {
	if v := os.Getenv("TMUX"); v != "" {
		return strings.SplitN(v, ",", 2)[0]
	}
	base := os.Getenv("TMUX_TMPDIR")
	if info, err := os.Stat(base); base == "" || err != nil || !info.IsDir() {
		base = "/tmp" // tmux's own fallback when TMUX_TMPDIR is unset or missing
	}
	return filepath.Join(base, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
}

// defaultTmuxSockets are every path that names the default server: $TMUX's,
// and default under $TMUX_TMPDIR, /tmp and /private/tmp, resolved.
func defaultTmuxSockets() map[string]bool {
	out := map[string]bool{}
	add := func(p string) {
		if p != "" {
			out[p], out[realPath(p)] = true, true
		}
	}
	if v := os.Getenv("TMUX"); v != "" {
		add(strings.SplitN(v, ",", 2)[0])
	}
	uid := fmt.Sprintf("tmux-%d", os.Getuid())
	for _, base := range []string{os.Getenv("TMUX_TMPDIR"), "/tmp", "/private/tmp"} {
		if base != "" {
			add(filepath.Join(base, uid, "default"))
		}
	}
	return out
}

// realPath resolves symlinks in p's directory (the socket itself may not
// exist yet).
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		return filepath.Join(dir, filepath.Base(p))
	}
	return p
}

// tmuxAt runs tmux against one server, always by -S.
type tmuxAt struct{ socket string }

func (t tmuxAt) run(args ...string) (string, error) {
	out, err := exec.Command("tmux", append([]string{"-S", t.socket}, args...)...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("tmux %s: %v: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

// hasSession reports whether name exists on the server. A server that is
// not running has no sessions.
func (t tmuxAt) hasSession(name string) bool {
	if _, err := os.Stat(t.socket); err != nil {
		return false
	}
	_, err := t.run("has-session", "-t", "="+name)
	return err == nil
}

// refuseExisting is the create-only pre-flight: nothing is created if any
// session the plan names already exists. It is also what makes window names
// unique enough for the daemon's placement check, which matches session and
// window name (hub issuedPlans.consume): every window it checks was created
// by this restore.
func (t tmuxAt) refuseExisting(plan restorePlan) error {
	var exist []string
	for _, s := range planSessions(plan) {
		if t.hasSession(s) {
			exist = append(exist, s)
		}
	}
	if len(exist) > 0 {
		return fmt.Errorf("session(s) %s already exist on %s; restore never reuses or changes a session, so nothing was created", strings.Join(exist, ", "), t.socket)
	}
	return nil
}

func planSessions(plan restorePlan) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range plan.Panes {
		if !seen[p.Session] {
			seen[p.Session] = true
			out = append(out, p.Session)
		}
	}
	return out
}

// applyPlan creates every session, window and pane in the plan's order,
// launching agents in their panes, then applies each window's layout. An
// agent whose path an earlier launch shares waits for that one to register
// first (Codex: its process start), so two sessions in one folder never race
// into twins. Returns what it placed.
func applyPlan(c *client, t tmuxAt, plan restorePlan, opts restoreOptions) ([]placedPane, error) {
	panes := append([]restorePane(nil), plan.Panes...)
	sort.SliceStable(panes, func(i, j int) bool {
		a, b := panes[i], panes[j]
		if a.Session != b.Session {
			return sessionOrder(plan, a.Session) < sessionOrder(plan, b.Session)
		}
		if a.WindowIndex != b.WindowIndex {
			return a.WindowIndex < b.WindowIndex
		}
		return a.PaneIndex < b.PaneIndex
	})
	var placed []placedPane
	lastAt := map[string]placedPane{} // path → the last agent launched there
	layouts := map[string]string{}
	created := map[string]bool{}
	lastPane := map[string]string{} // window → its newest pane, split next so panes keep their order
	for _, p := range panes {
		window := p.Session + ":" + strconv.Itoa(p.WindowIndex)
		cmd := ""
		if p.Action == "launch" {
			cmd = p.Command
			if prev, ok := lastAt[p.CWD]; ok {
				waitRegistered(c, prev, opts)
			}
		}
		paneID, err := t.place(p, window, created, lastPane[window], cmd)
		if err != nil {
			return placed, err
		}
		lastPane[window] = paneID
		layouts[window] = p.WindowLayout
		if p.Action == "launch" {
			pl := placedPane{PlanID: plan.PlanID, PaneID: paneID, PeerID: p.PeerID, TmuxSession: p.Session}
			if _, err := c.request(http.MethodPost, "/swarm/placed", pl); err != nil {
				fmt.Fprintf(os.Stderr, "repowire: %s launched in %s, but the daemon did not record it: %v\n", p.PeerID, paneID, err)
			}
			placed = append(placed, pl)
			lastAt[p.CWD] = pl
		}
	}
	for window, layout := range layouts {
		if layout == "" {
			continue
		}
		if _, err := t.run("select-layout", "-t", window, layout); err != nil {
			fmt.Fprintf(os.Stderr, "repowire: %s keeps tmux's default layout: %v\n", window, err)
		}
	}
	return placed, nil
}

func sessionOrder(plan restorePlan, name string) int {
	for i, s := range planSessions(plan) {
		if s == name {
			return i
		}
	}
	return len(plan.Panes)
}

// place creates p's pane: its session's first window with new-session, a
// window's first pane with new-window at its explicit index, every other
// pane by splitting the window's newest pane (after), so pane indices follow
// the plan's order. A launch pane runs its command from the start; a shell
// pane runs the default shell in its cwd.
func (t tmuxAt) place(p restorePane, window string, created map[string]bool, after, cmd string) (string, error) {
	var args []string
	switch {
	case !created[p.Session]:
		args = []string{"new-session", "-d", "-s", p.Session, "-n", p.WindowName, "-c", p.CWD, "-P", "-F", "#{pane_id}\t#{window_index}"}
	case !created[window]:
		args = []string{"new-window", "-d", "-t", window, "-n", p.WindowName, "-c", p.CWD, "-P", "-F", "#{pane_id}\t#{window_index}"}
	default:
		args = []string{"split-window", "-d", "-t", after, "-c", p.CWD, "-P", "-F", "#{pane_id}\t#{window_index}"}
	}
	if cmd != "" {
		args = append(args, cmd)
	}
	out, err := t.run(args...)
	if err != nil {
		return "", err
	}
	paneID, index, _ := strings.Cut(out, "\t")
	if !created[p.Session] && index != strconv.Itoa(p.WindowIndex) {
		// new-session numbers its first window from base-index; move it to
		// the recorded index (the session was created just now, so this
		// moves nothing that existed).
		if _, err := t.run("move-window", "-s", p.Session+":"+index, "-t", window); err != nil {
			return "", err
		}
	}
	created[p.Session], created[window] = true, true
	return paneID, nil
}

// waitRegistered waits, bounded, for prev to register before the next
// agent at its path starts. Codex registers on its first turn, so for it the
// started process is enough.
func waitRegistered(c *client, prev placedPane, opts restoreOptions) {
	deadline := time.Now().Add(opts.register)
	for time.Now().Before(deadline) {
		v := verifyOnce(c, []placedPane{prev})
		if len(v) == 1 && (v[0].Outcome == "back" || v[0].Outcome == "awaits_first_turn" || v[0].Outcome == "twin") {
			return
		}
		time.Sleep(opts.poll)
	}
	fmt.Fprintf(os.Stderr, "repowire: %s has not registered after %s; starting the next agent at its path anyway\n", prev.PeerID, opts.register)
}

func verifyOnce(c *client, placed []placedPane) []verifiedAgent {
	result, err := c.request(http.MethodPost, "/swarm/verify", map[string]any{"placed": placed})
	if err != nil {
		return nil
	}
	var body struct {
		Agents []verifiedAgent `json:"agents"`
	}
	if remarshal(result, &body) != nil {
		return nil
	}
	return body.Agents
}

// verifyPlaced polls, bounded, until no placed agent is still missing.
func verifyPlaced(c *client, placed []placedPane, opts restoreOptions) []verifiedAgent {
	if len(placed) == 0 {
		return nil
	}
	deadline := time.Now().Add(opts.register)
	for {
		v := verifyOnce(c, placed)
		missing := false
		for _, a := range v {
			missing = missing || a.Outcome == "missing"
		}
		if (!missing && v != nil) || !time.Now().Before(deadline) {
			return v
		}
		time.Sleep(opts.poll)
	}
}
