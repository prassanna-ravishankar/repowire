package hooks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitHookProcess(t *testing.T) {
	if os.Getenv("REPOWIRE_TEST_GIT_HOOK") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Exit(runPrepareCommitMsg(os.Args[i+1:]))
		}
	}
	os.Exit(2)
}

func TestNativeGitTrailers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("REPOWIRE_EXPERIMENTS__GIT_TRAILERS", "true")
	t.Setenv("TMUX_PANE", "%7")
	t.Setenv("REPOWIRE_TEST_GIT_HOOK", "1")
	requests := make(chan string, 20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Query().Get("since")
		_ = json.NewEncoder(w).Encode(map[string]any{"threads": []string{"ask-old", "ask-new"}, "repowire_session_id": "rw-session"})
	}))
	defer server.Close()
	configureHookTestDaemon(t, server.URL)
	git := func(repo string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	repo := t.TempDir()
	git(repo, "init", "-q")
	git(repo, "config", "user.name", "Test")
	git(repo, "config", "user.email", "test@example.invalid")
	git(repo, "config", "commit.gpgsign", "false")
	git(repo, "commit", "--allow-empty", "-m", "base", "--trailer", "Repowire-Thread: ask-old")
	since := git(repo, "log", "-1", "--format=%cI")
	exe, _ := os.Executable()
	hook := "#!/bin/sh\nexec '" + strings.ReplaceAll(exe, "'", "'\"'\"'") + "' -test.run=^TestGitHookProcess$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(repo, ".git/hooks/prepare-commit-msg"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	// Commit from a different cwd, as with cd other-repo && git commit.
	git(t.TempDir(), "-C", repo, "commit", "--allow-empty", "-m", "new")
	if got := <-requests; got != since {
		t.Fatalf("since=%q want target repository %q", got, since)
	}
	message := git(repo, "log", "-1", "--format=%B")
	if !strings.Contains(message, "Repowire-Thread: ask-new") || strings.Contains(message, "ask-old") || !strings.Contains(message, "Repowire-Session: rw-session") {
		t.Fatal(message)
	}
	git(repo, "commit", "--amend", "--no-edit", "--allow-empty")
	if got := git(repo, "log", "-1", "--format=%B"); got != message {
		t.Fatalf("amend changed message: %s", got)
	}
	git(repo, "commit", "--allow-empty", "-m", "second")
	<-requests
	if got := git(repo, "log", "-1", "--format=%B"); strings.Contains(got, "ask-new") {
		t.Fatalf("duplicate: %s", got)
	}
	// No Git invocation means no transformation of shell literals or heredocs.
	cmd := exec.Command("sh", "-c", "printf '%s\\n' 'git commit' > literal; cat <<'END' > heredoc\ngit commit\nEND\n# git commit\n")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shell: %v %s", err, out)
	}
	for _, name := range []string{"literal", "heredoc"} {
		data, _ := os.ReadFile(filepath.Join(repo, name))
		if string(data) != "git commit\n" {
			t.Fatal(string(data))
		}
	}
	// Existing user attribution is preserved.
	git(repo, "commit", "--allow-empty", "-m", "manual", "--trailer", "Repowire-Session: original")
	if got := git(repo, "log", "-1", "--format=%B"); strings.Contains(got, "rw-session") {
		t.Fatal(got)
	}
	// An empty message must still abort instead of becoming metadata-only.
	empty := exec.Command("git", "commit", "--allow-empty", "-m", "")
	empty.Dir = repo
	if out, err := empty.CombinedOutput(); err == nil {
		t.Fatalf("empty message committed: %s", out)
	}
	// Rebase replay with an explicitly supplied message must not add attribution.
	rebase := filepath.Join(repo, ".git", "rebase-merge")
	if err := os.Mkdir(rebase, 0o755); err != nil {
		t.Fatal(err)
	}
	git(repo, "commit", "--allow-empty", "-m", "replayed")
	if got := git(repo, "log", "-1", "--format=%B"); got != "replayed" {
		t.Fatal(got)
	}
	if err := os.Remove(rebase); err != nil {
		t.Fatal(err)
	}
	// Linked worktrees use the same installed hook and their own HEAD.
	worktree := filepath.Join(t.TempDir(), "linked")
	git(repo, "worktree", "add", "-b", "linked", worktree)
	git(worktree, "commit", "--allow-empty", "-m", "linked change")
	<-requests
	if got := git(worktree, "log", "-1", "--format=%B"); !strings.Contains(got, "Repowire-Session: rw-session") {
		t.Fatal(got)
	}
	// An unborn repository has no previous commit timestamp.
	initial := t.TempDir()
	git(initial, "init", "-q")
	git(initial, "config", "user.name", "Test")
	git(initial, "config", "user.email", "test@example.invalid")
	git(initial, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(initial, ".git/hooks/prepare-commit-msg"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	git(initial, "commit", "--allow-empty", "-m", "initial")
	if got := <-requests; got != "" {
		t.Fatalf("initial since: %q", got)
	}
	if got := git(initial, "log", "-1", "--format=%B"); !strings.Contains(got, "Repowire-Thread: ask-new") {
		t.Fatal(got)
	}
	t.Setenv("REPOWIRE_EXPERIMENTS__GIT_TRAILERS", "false")
	git(repo, "commit", "--allow-empty", "-m", "disabled")
	if got := git(repo, "log", "-1", "--format=%B"); strings.Contains(got, "Repowire-") {
		t.Fatal(got)
	}
	select {
	case got := <-requests:
		t.Fatalf("unexpected history query: %s", got)
	default:
	}
	t.Setenv("REPOWIRE_EXPERIMENTS__GIT_TRAILERS", "true")
	server.Close()
	git(repo, "commit", "--allow-empty", "-m", "offline")
	if got := git(repo, "log", "-1", "--format=%B"); got != "offline" {
		t.Fatal(got)
	}
}
