package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
)

// The cases mirror codex-cli 0.171 tui/src/daemon_startup.rs: which launch
// options keep a TUI on the shared App Server the bridge attaches to (#567).
func TestCodexEmbeddedReason(t *testing.T) {
	cases := []struct {
		command string
		env     map[string]string
		want    string
	}{
		{command: "codex --dangerously-bypass-approvals-and-sandbox"},
		{command: "codex --model gpt-5 -s workspace-write -a never --enable foo --disable bar"},
		{command: "codex -c features.foo=true -c tui.fullscreen_transcript=false --config=suppress_unstable_features_warning=true"},
		{command: "codex resume --last"},
		{command: "codex -- --no-daemon"}, // after `--` it is the prompt
		{command: "codex -m -p"},          // a value, not the profile flag
		{command: "codex --no-daemon", want: "--no-daemon"},
		{command: "codex --oss", want: "--oss"},
		{command: "codex --search", want: "--search"},
		{command: "codex --strict-config", want: "--strict-config"},
		{command: "codex --dangerously-bypass-hook-trust", want: "--dangerously-bypass-hook-trust"},
		{command: "codex -p work", want: "-p"},
		{command: "codex --profile=work", want: "--profile"},
		{command: "codex resume --last -p work", want: "-p"},
		{command: "codex -c sandbox_mode=workspace-write", want: "-c sandbox_mode"},
		{command: "codex --config model_reasoning_effort=high", want: "-c model_reasoning_effort"},
		{command: "codex -cmodel=gpt-5", want: "-c model"},
		{command: `codex -c 'features.foo="true"'`, want: "-c features.foo"}, // a TOML string, not a bool
		{command: "codex -c features.a.b=true", want: "-c features.a.b"},
		{command: "codex -c 'model_providers.x.api_key=sk-secret'", want: "-c model_providers.x.api_key"},
		{command: "env CODEX_EXEC_SERVER_URL=ws://x codex", want: "CODEX_EXEC_SERVER_URL"},
		{command: "codex", env: map[string]string{"CODEX_EXEC_SERVER_URL": "ws://x"}, want: "CODEX_EXEC_SERVER_URL"},
		{command: "OPENAI_IDENTITY_TOKEN_FILE=/t codex", want: "OPENAI_IDENTITY_TOKEN_FILE"},
		{command: "codex --approve-for-me", want: "--approve-for-me"},
		{command: "codex --not-so-yolo", want: "--not-so-yolo"},
		{command: "codex -pwork", want: "-p"},
		{command: "codex -c=features.foo=true"},
		{command: "codex -mgpt-5"},
		{command: "codex -c 'features.foo=true # on'"},
		{command: "codex -c 'features={a=true, b=false}'"},
		{command: "codex -c 'tui={fullscreen_transcript=true}'"},
		{command: `codex -c 'tui={"fullscreen_transcript"=true}'`},
		{command: `codex -c 'features={"shell_tool"=true}'`},
		{command: "codex -c 'features={a.b=true}'", want: "-c features"},
		{command: "codex -c 'features={}'", want: "-c features"},
		{command: "codex -c 'features={a=\"x\"}'", want: "-c features"},
		{command: "codex -c 'tui={notifications=false}'", want: "-c tui"},
		{command: "codex -c 'tui={fullscreen_transcript=true, x=true}'", want: "-c tui"},
	}
	for _, c := range cases {
		if got := CodexEmbeddedReason(c.command, c.env); got != c.want {
			t.Errorf("CodexEmbeddedReason(%q) = %q, want %q", c.command, got, c.want)
		}
	}
}

func TestCodexHooksTransportReadsInstalledHooks(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	if codexHooksTransport(nil) {
		t.Fatal("no hooks.json: want App Server transport")
	}
	write := func(command string) {
		body := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"repowire ` + command + `"}]}]}}`
		if err := os.WriteFile(filepath.Join(root, "hooks.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("hook stop --backend=codex --reminders-only")
	if codexHooksTransport(nil) {
		t.Fatal("reminder-only Stop hook is the App Server install")
	}
	write("hook session --backend=codex")
	if !codexHooksTransport(nil) {
		t.Fatal("SessionStart hook installed: want hooks transport")
	}
	// The launch's own CODEX_HOME wins over the daemon's.
	if codexHooksTransport(map[string]string{"CODEX_HOME": t.TempDir()}) {
		t.Fatal("launch CODEX_HOME without hooks: want App Server transport")
	}
}

func TestResolveCommandRefusesEmbeddedCodexOnAppServer(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	onHooks := false
	svc := NewSpawnService(nil, nil, map[proto.AgentType]string{proto.AgentCodex: "codex --dangerously-bypass-approvals-and-sandbox"}, nil).
		WithRuntimeConfig(map[proto.AgentType]map[string][]string{proto.AgentCodex: {
			"fast":    {"--model", "gpt-5-mini"},
			"careful": {"-c", "sandbox_mode=workspace-write"},
		}}, map[string]string{"PATH": bin})
	svc.codexOnHooks = func() bool { return onHooks }

	for _, profile := range []string{"", "fast"} {
		if _, err := svc.ResolveCommand(proto.AgentCodex, &profile); err != nil {
			t.Fatalf("profile %q keeps the shared App Server: %v", profile, err)
		}
	}
	careful := "careful"
	_, err := svc.ResolveCommand(proto.AgentCodex, &careful)
	var spawnErr *SpawnError
	if !errors.As(err, &spawnErr) || spawnErr.Status != 422 {
		t.Fatalf("embedded launch: want 422, got %v", err)
	}
	detail, _ := spawnErr.Detail.(map[string]any)
	hint, _ := detail["hint"].(string)
	if detail["error"] != "codex_embedded_app_server" || detail["reason"] != "-c sandbox_mode" ||
		!strings.Contains(hint, "daemon.spawn.profiles.codex.careful") || strings.Contains(hint, "workspace-write") {
		t.Fatalf("refusal detail = %#v", detail)
	}

	onHooks = true
	if _, err := svc.ResolveCommand(proto.AgentCodex, &careful); err != nil {
		t.Fatalf("hooks transport reaches embedded sessions: %v", err)
	}
	svc.commands[proto.AgentClaudeCode] = "claude -c"
	onHooks = false
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveCommand(proto.AgentClaudeCode, nil); err != nil {
		t.Fatalf("only codex launches are checked: %v", err)
	}
}

// The hooks probe reads the Codex home the launch itself uses.
func TestCheckCodexLaunchUsesTheLaunchCodexHome(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir()) // the daemon's: no hooks
	hooksHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(hooksHome, "hooks.json"), []byte(`{"hooks":{"SessionStart":[{"hooks":[{"command":"repowire hook session --backend=codex"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewSpawnService(nil, nil, nil, nil)
	if err := svc.CheckCodexLaunch("codex -p work", "test"); !IsCodexEmbeddedRefusal(err) {
		t.Fatalf("daemon home without hooks: want refusal, got %v", err)
	}
	if err := svc.CheckCodexLaunch("CODEX_HOME="+hooksHome+" codex -p work", "test"); err != nil {
		t.Fatalf("launch home on the hooks transport: %v", err)
	}
	svc.WithRuntimeConfig(nil, map[string]string{"CODEX_HOME": hooksHome})
	if err := svc.CheckCodexLaunch("codex -p work", "test"); err != nil {
		t.Fatalf("spawn env home on the hooks transport: %v", err)
	}
}
