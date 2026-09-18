package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type exitCode int

func (e exitCode) Error() string { return "" }

func executeRoot(argv []string) int {
	root := newRootCommand()
	root.SetArgs(argv)
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	if err := root.Execute(); err != nil {
		if code, ok := err.(exitCode); ok {
			return int(code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	return 0
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "repowire",
		Short:         "Mesh network for AI coding agents",
		Version:       Version,
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetHelpFunc(renderCommandHelp)

	root.AddCommand(
		legacy("setup [flags]", "Install Repowire and connect detected agent runtimes", nil, runSetup),
		simple("status", "Show daemon, runtime, integration, and update status", runStatus),
		simple("doctor", "Check Repowire and host prerequisites", runDoctor),
		legacy("link --pane PANE --backend BACKEND [flags]", "Link an existing tmux pane to the mesh", nil, runLink),
		legacy("trace TRACE_ID [--json]", "Show delivery stages for a message", nil, runTrace),
		legacy("why [COMMIT] [--json]", "Show the mesh conversations behind a commit", nil, runWhy),
		legacy("share PEER [flags]", "Create, list, or revoke relay share links", nil, runShare),
		simple("update", "Update Repowire to the latest release", runUpdate),
		legacy("uninstall [flags]", "Remove Repowire integrations", nil, runUninstall),
		&cobra.Command{Use: "version", Short: "Print the Repowire version", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), Version)
			return nil
		}},
	)

	root.AddCommand(group("peer", "Inspect and control mesh peers", runPeer, []leaf{
		{"list [--show-offline]", "List peers"}, {"describe NAME_OR_ID [flags]", "Show one peer"}, {"new PATH [flags]", "Spawn a peer"},
		{"register [flags]", "Register a peer"}, {"unregister NAME_OR_ID", "Retire a peer"}, {"restart NAME_OR_ID [flags]", "Restart and resume a peer"},
		{"doctor NAME_OR_ID [flags]", "Diagnose a peer"}, {"rehook NAME_OR_ID [flags]", "Repair inbound delivery"}, {"ask NAME QUERY [flags]", "Send a blocking compatibility ask"},
		{"prune [flags]", "Remove offline peers"}, {"whoami [flags]", "Show the current peer identity"}, {"asks [flags]", "List pending asks"},
		{"deliveries [flags]", "Drain queued deliveries"}, {"ack CORRELATION_ID [flags]", "Acknowledge an ask"}, {"decline CORRELATION_ID [flags]", "Return an ask to its asker"}, {"claim-role orchestrator [flags]", "Claim the orchestrator role"},
	}))
	root.AddCommand(group("jobs", "Create and control durable jobs", runJobs, []leaf{
		{"create TITLE [flags]", "Create a job"}, {"list [flags]", "List jobs"}, {"show JOB_ID [--json]", "Show a job"}, {"update JOB_ID [flags]", "Update a job"},
		{"run JOB_ID [--json]", "Run a job"}, {"retry JOB_ID [--json]", "Retry a job"}, {"cancel JOB_ID [flags]", "Cancel a job"}, {"result JOB_ID [--json]", "Show a job result"},
	}))
	root.AddCommand(group("schedule", "Manage scheduled mesh messages", runSchedule, []leaf{
		{"self WHEN_OR_CRON TEXT [flags]", "Schedule a message to this peer"}, {"create PEER WHEN_OR_CRON TEXT [flags]", "Schedule a message to another peer"},
		{"list [flags]", "List schedules"}, {"delete SCHEDULE_ID", "Delete a schedule"},
	}))
	root.AddCommand(group("session", "Control durable sessions", runSession, []leaf{{"resume SESSION_ID [flags]", "Resume a durable session"}}))
	root.AddCommand(group("service", "Manage installed user services", runService, []leaf{
		{"install", "Install and start services"}, {"start", "Start services"}, {"restart [daemon|bridge|all]", "Restart services"}, {"status", "Show service status"}, {"uninstall", "Remove services"},
	}))
	root.AddCommand(group("daemon", "Manage the routing daemon", runDaemonCommand, []leaf{
		{"start [--foreground]", "Start the daemon"}, {"stop", "Stop the daemon"}, {"status", "Show daemon status"},
	}))
	root.AddCommand(group("config", "Inspect Repowire configuration", runConfig, []leaf{
		{"path", "Print the config path"}, {"show", "Print the config file"}, {"get KEY", "Read one config value"},
	}))
	root.AddCommand(group("relay", "Run the hosted relay server", runRelay, []leaf{
		{"start [flags]", "Start the relay server"}, {"generate-key [--user-id ID]", "Generate a relay API key"},
	}))
	root.AddCommand(group("agents", "Create durable agent workspaces", runAgents, []leaf{{"create NAME [flags]", "Create an agent workspace"}}))
	root.AddCommand(group("memory", "Manage Repowire memory files", runMemory, []leaf{
		{"path", "Print the memory path"}, {"list [flags]", "List memory files"}, {"show NAME", "Show a memory file"}, {"search QUERY [flags]", "Search memory"},
		{"write NAME --body TEXT [flags]", "Write memory"},
	}))
	root.AddCommand(orchestratorCommand())
	for _, runtime := range []string{"claude", "codex", "opencode", "pi"} {
		backend := runtime
		if backend == "claude" {
			backend = "claude-code"
		}
		root.AddCommand(group(runtime, "Manage the "+runtime+" integration", func(args []string) int { return runRuntimeInstall(backend, args) }, []leaf{
			{"install", "Install the integration"}, {"uninstall", "Remove the integration"}, {"status", "Show integration status"},
		}))
	}
	root.AddCommand(group("hooks", "Manage Claude Code hooks", func(args []string) int { return runRuntimeInstall("claude-code", args) }, []leaf{
		{"install", "Install hooks"}, {"uninstall", "Remove hooks"}, {"status", "Show hook status"},
	}))
	for _, bot := range []string{"telegram", "slack"} {
		name := bot
		root.AddCommand(group(name, "Run the "+name+" mesh peer", func(args []string) int { return runBot(name, args) }, []leaf{{"start", "Start the bot peer"}}))
	}
	serve := &cobra.Command{Use: "serve [flags]", Short: "Run the daemon in the foreground", RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	root.AddCommand(serve)
	buildUI := simple("build-ui", "Build the embedded web dashboard", runBuildUI)
	buildUI.Hidden = true
	root.AddCommand(buildUI)
	return root
}

type leaf struct{ use, short string }

func group(use, short string, run func([]string) int, leaves []leaf) *cobra.Command {
	cmd := &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	for _, item := range leaves {
		prefix := item.use
		if i := len(item.use); i > 0 {
			for n, r := range item.use {
				if r == ' ' {
					prefix = item.use[:n]
					break
				}
			}
		}
		child := legacy(item.use, item.short, []string{prefix}, run)
		documentFlags(child, use+" "+prefix)
		cmd.AddCommand(child)
	}
	return cmd
}

func orchestratorCommand() *cobra.Command {
	cmd := group("orchestrator", "Manage the durable orchestrator", runOrchestrator, []leaf{
		{"init [--force]", "Initialize the orchestrator workspace"}, {"diff", "Show orchestrator template drift"}, {"start [flags]", "Start the orchestrator"},
	})
	cmd.AddCommand(group("persona", "Manage orchestrator personas", func(args []string) int { return runOrchestrator(append([]string{"persona"}, args...)) }, []leaf{
		{"list", "List personas"}, {"show NAME", "Show a persona"}, {"use NAME", "Select a persona"}, {"clear", "Clear the selected persona"},
	}))
	return cmd
}

func simple(use, short string, run func() int) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error { return codeError(run()) }}
}

func legacy(use, short string, prefix []string, run func([]string) int) *cobra.Command {
	cmd := &cobra.Command{Use: use, Short: short, DisableFlagParsing: true}
	documentFlags(cmd, strings.Fields(use)[0])
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		for _, arg := range args {
			if arg == "help" || arg == "--help" || arg == "-h" {
				return cmd.Help()
			}
		}
		return codeError(run(append(prefix, args...)))
	}
	return cmd
}

type documentedFlag struct {
	name, shorthand, usage string
	boolean                bool
}

func documentFlags(cmd *cobra.Command, key string) {
	for _, flag := range documentedFlags[key] {
		if flag.boolean {
			cmd.Flags().BoolP(flag.name, flag.shorthand, false, flag.usage)
		} else {
			cmd.Flags().StringP(flag.name, flag.shorthand, "", flag.usage)
		}
	}
}

var documentedFlags = map[string][]documentedFlag{
	"setup": {
		{name: "git-hooks", usage: "Install only the current repository commit-message hook", boolean: true},
		{name: "relay", usage: "Enable the hosted relay", boolean: true}, {name: "experimental-channels", usage: "Enable the experimental Claude channel transport", boolean: true},
		{name: "http-mcp", usage: "Accepted for compatibility; HTTP MCP is enabled by default", boolean: true}, {name: "update-checks", usage: "Enable release update checks", boolean: true},
		{name: "no-update-checks", usage: "Disable release update checks", boolean: true}, {name: "no-service", usage: "Skip user-service installation", boolean: true}, {name: "non-interactive", usage: "Use defaults without prompting", boolean: true},
	},
	"link":               {{name: "pane", usage: "tmux pane id"}, {name: "backend", usage: "Agent backend"}, {name: "name", usage: "Display name"}, {name: "circle", usage: "Circle"}, {name: "cwd", usage: "Working directory"}},
	"trace":              {{name: "json", usage: "Emit JSON", boolean: true}},
	"why":                {{name: "json", usage: "Emit JSON", boolean: true}},
	"share":              {{name: "rw", usage: "Create a read-write share", boolean: true}, {name: "list", usage: "List active shares", boolean: true}, {name: "ttl", usage: "Lifetime in seconds"}, {name: "revoke", usage: "Revoke a share id"}},
	"uninstall":          {{name: "yes", usage: "Also remove Repowire state", boolean: true}},
	"peer list":          {{name: "show-offline", usage: "Include offline peers", boolean: true}, {name: "include-hidden", usage: "Include peers hidden from the default view (no direct input, runtime-internal threads, sub-agents with an offline parent)", boolean: true}, {name: "source", usage: "Only peers from this source: hook, codex-app-server, unknown"}},
	"peer describe":      {{name: "circle", usage: "Circle used to disambiguate the name"}},
	"peer new":           {{name: "backend", usage: "Agent backend"}, {name: "profile", usage: "Spawn profile"}, {name: "circle", usage: "Target circle"}, {name: "message", shorthand: "m", usage: "Initial prompt"}, {name: "command", usage: "Deprecated explicit launch command"}},
	"peer register":      {{name: "backend", usage: "Agent backend"}, {name: "name", usage: "Display name"}, {name: "circle", usage: "Circle"}, {name: "path", usage: "Working directory"}},
	"peer restart":       {{name: "circle", usage: "Circle used to disambiguate the name"}, {name: "dry-run", usage: "Validate without restarting", boolean: true}, {name: "message", shorthand: "m", usage: "Resume prompt"}},
	"peer doctor":        {{name: "circle", usage: "Circle used to disambiguate the name"}, {name: "json", usage: "Emit JSON", boolean: true}, {name: "fix", usage: "Apply safe inbound repair", boolean: true}},
	"peer rehook":        {{name: "circle", usage: "Circle used to disambiguate the name"}, {name: "apply", usage: "Apply the repair", boolean: true}},
	"peer ask":           {{name: "circle", usage: "Circle used to resolve the peer"}, {name: "timeout", usage: "Wait timeout in seconds"}},
	"peer prune":         {{name: "dry-run", usage: "Show what would be removed", boolean: true}, {name: "force", usage: "Skip confirmation", boolean: true}},
	"peer whoami":        {{name: "register", usage: "Register before resolving", boolean: true}, {name: "peer-id", usage: "Peer id"}, {name: "name", usage: "Display name"}, {name: "backend", usage: "Agent backend"}, {name: "circle", usage: "Circle"}, {name: "path", usage: "Working directory"}},
	"peer asks":          {{name: "peer-id", usage: "Peer id"}, {name: "pane-id", usage: "tmux pane id"}, {name: "peer", usage: "Display name"}, {name: "direction", usage: "inbound, outbound, or both"}, {name: "json", usage: "Emit JSON", boolean: true}},
	"peer deliveries":    {{name: "peer-id", usage: "Peer id"}, {name: "pane-id", usage: "tmux pane id"}, {name: "peer", usage: "Display name"}, {name: "json", usage: "Emit JSON", boolean: true}},
	"peer ack":           {{name: "message", shorthand: "m", usage: "Reply message"}, {name: "from-peer", usage: "Sender identity"}},
	"peer decline":       {{name: "reason", shorthand: "r", usage: "Reason the ask cannot be handled"}, {name: "from-peer", usage: "Sender identity"}},
	"peer claim-role":    {{name: "peer", usage: "Peer id or display name"}, {name: "circle", usage: "Circle"}, {name: "force", usage: "Replace a stale holder", boolean: true}},
	"jobs create":        {{name: "kind", usage: "Job kind"}, {name: "prompt", usage: "Prompt text"}, {name: "prompt-file", usage: "Prompt file"}, {name: "assigned-peer", usage: "Assigned peer"}, {name: "owner", usage: "Owner peer id"}, {name: "path", usage: "Worker path"}, {name: "backend", usage: "Worker backend"}, {name: "profile", usage: "Spawn profile"}, {name: "due-at", usage: "One-shot due time"}, {name: "cron", usage: "Cron expression"}, {name: "process-scope", usage: "per-fire or persistent"}, {name: "continuity", usage: "fresh or resume"}, {name: "result-surface", usage: "Result destination"}, {name: "circle", usage: "Circle"}, {name: "visibility", usage: "Visibility scope"}, {name: "json", usage: "Emit JSON", boolean: true}},
	"jobs list":          {{name: "state", usage: "Filter by state"}, {name: "owner", usage: "Filter by owner"}, {name: "created-by", usage: "Filter by creator"}, {name: "session", usage: "Filter by session"}, {name: "circle", usage: "Filter by circle"}, {name: "json", usage: "Emit JSON", boolean: true}},
	"jobs show":          {{name: "json", usage: "Emit JSON", boolean: true}},
	"jobs update":        {{name: "state", usage: "New lifecycle state"}, {name: "attempt-id", usage: "Attempt id"}, {name: "reason", usage: "State reason"}, {name: "phase", usage: "Current phase"}, {name: "note", usage: "Progress note"}, {name: "result-summary", usage: "Result summary"}, {name: "json", usage: "Emit JSON", boolean: true}},
	"jobs run":           {{name: "json", usage: "Emit JSON", boolean: true}},
	"jobs retry":         {{name: "json", usage: "Emit JSON", boolean: true}},
	"jobs cancel":        {{name: "requested-by", usage: "Requester peer id"}, {name: "reason", usage: "Cancellation reason"}, {name: "json", usage: "Emit JSON", boolean: true}},
	"jobs result":        {{name: "json", usage: "Emit JSON", boolean: true}},
	"session resume":     {{name: "dry-run", usage: "Validate without resuming", boolean: true}, {name: "profile", usage: "Spawn profile"}, {name: "message", shorthand: "m", usage: "Resume prompt"}, {name: "json", usage: "Emit JSON", boolean: true}},
	"schedule self":      {{name: "cron", usage: "Treat time as a cron expression", boolean: true}, {name: "kind", usage: "notify or ask"}, {name: "circle", usage: "Circle"}},
	"schedule create":    {{name: "cron", usage: "Treat time as a cron expression", boolean: true}, {name: "kind", usage: "notify or ask"}, {name: "circle", usage: "Circle"}, {name: "from-peer", usage: "Sender identity"}},
	"schedule list":      {{name: "from-peer", usage: "Filter by sender"}},
	"daemon start":       {{name: "foreground", usage: "Run in the foreground", boolean: true}},
	"relay start":        {{name: "host", usage: "Bind host"}, {name: "port", usage: "Bind port"}},
	"relay generate-key": {{name: "user-id", usage: "User id embedded in the key"}},
	"agents create":      {{name: "path", usage: "Workspace path"}, {name: "backend", usage: "Default backend"}, {name: "force", usage: "Overwrite an existing workspace", boolean: true}, {name: "json", usage: "Emit JSON", boolean: true}},
	"memory list":        {{name: "scope", usage: "Memory scope"}, {name: "project", usage: "Project scope name"}, {name: "persona", usage: "Persona scope name"}},
	"memory search":      {{name: "all", usage: "Search every memory scope", boolean: true}, {name: "scope", usage: "Memory scope"}, {name: "project", usage: "Project scope name"}, {name: "persona", usage: "Persona scope name"}},
	"memory write":       {{name: "body", usage: "Memory contents"}, {name: "description", usage: "Short description"}, {name: "type", usage: "Memory type"}, {name: "append", usage: "Append to an existing file", boolean: true}, {name: "force", usage: "Overwrite an existing file", boolean: true}, {name: "scope", usage: "Memory scope"}, {name: "project", usage: "Project scope name"}, {name: "persona", usage: "Persona scope name"}},
	"orchestrator init":  {{name: "force", usage: "Replace the existing workspace", boolean: true}},
	"orchestrator start": {{name: "runtime", usage: "Agent runtime"}, {name: "circle", usage: "Circle"}, {name: "profile", usage: "Spawn profile"}, {name: "service", usage: "Reserved service mode", boolean: true}},
}

func codeError(code int) error {
	if code == 0 {
		return nil
	}
	return exitCode(code)
}

func renderCommandHelp(cmd *cobra.Command, _ []string) {
	w := cmd.OutOrStdout()
	width := helpWidth(w)
	if cmd.Short != "" {
		fmt.Fprintln(w, cmd.Short)
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintf(w, "  %s", cmd.CommandPath())
	if tail := strings.TrimPrefix(cmd.Use, cmd.Name()); tail != "" {
		fmt.Fprint(w, tail)
	} else if cmd.HasAvailableSubCommands() {
		fmt.Fprint(w, " [command]")
	}
	fmt.Fprintln(w)

	if cmd.HasAvailableSubCommands() {
		fmt.Fprintln(w, "\nCommands:")
		nameWidth := 0
		for _, child := range cmd.Commands() {
			if child.IsAvailableCommand() && len(child.Name()) > nameWidth {
				nameWidth = len(child.Name())
			}
		}
		for _, child := range cmd.Commands() {
			if !child.IsAvailableCommand() {
				continue
			}
			indent := nameWidth + 4
			lines := wrapWords(child.Short, max(20, width-indent))
			fmt.Fprintf(w, "  %-*s  %s\n", nameWidth, child.Name(), lines[0])
			for _, line := range lines[1:] {
				fmt.Fprintf(w, "%*s%s\n", indent, "", line)
			}
		}
	}
	if flags := strings.TrimRight(cmd.LocalFlags().FlagUsagesWrapped(max(20, width-2)), "\n"); strings.TrimSpace(flags) != "" {
		fmt.Fprintln(w, "\nFlags:")
		fmt.Fprintln(w, flags)
	}
	if cmd.HasAvailableSubCommands() {
		fmt.Fprintf(w, "\nUse %q for more information.\n", cmd.CommandPath()+" [command] --help")
	}
}

func helpWidth(w io.Writer) int {
	if file, ok := w.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		if width, _, err := term.GetSize(int(file.Fd())); err == nil && width > 0 {
			return width
		}
	}
	return 80
}

func wrapWords(value string, width int) []string {
	words := strings.Fields(value)
	if len(words) == 0 {
		return []string{""}
	}
	lines := []string{words[0]}
	for _, word := range words[1:] {
		last := len(lines) - 1
		if len([]rune(lines[last]))+1+len([]rune(word)) <= width {
			lines[last] += " " + word
		} else {
			lines = append(lines, word)
		}
	}
	return lines
}
