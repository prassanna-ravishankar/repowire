package hooks

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/repowire/repowire/daemon-go/config"
)

// Git supplies the repository cwd/environment and message file, never shell text.
// Attribution is best effort: warn on failure but never prevent a commit.
func runPrepareCommitMsg(args []string) int {
	if err := prepareCommitMsg(args); err != nil {
		errf("git trailers: %v", err)
	}
	return 0
}

func prepareCommitMsg(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing commit message file")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if !cfg.Experiments.GitTrailers {
		return nil
	}
	if len(args) > 1 && args[1] != "" && args[1] != "message" && args[1] != "template" {
		return nil
	}
	pane := getPaneID()
	if pane == "" || gitOutput("", "rev-parse", "--is-inside-work-tree") != "true" {
		return nil
	}
	// Do not attribute replayed work to the currently active peer.
	for _, name := range []string{"rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD", "REVERT_HEAD", "MERGE_HEAD"} {
		path := gitOutput("", "rev-parse", "--git-path", name)
		if path != "" {
			if _, err := os.Stat(path); err == nil {
				return nil
			}
		}
	}
	original, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	// Existing attribution belongs to its original author (including amend/reuse).
	lower := strings.ToLower(string(original))
	if strings.Contains(lower, "repowire-thread:") || strings.Contains(lower, "repowire-session:") {
		return nil
	}
	// Do not turn an empty message into a valid commit solely by adding metadata.
	clean := exec.Command("git", "stripspace", "--strip-comments")
	clean.Stdin = strings.NewReader(string(original))
	stripped, err := clean.Output()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(stripped)) == "" {
		return nil
	}
	query := "pane_id=" + url.QueryEscape(pane)
	if since := gitOutput("", "log", "-1", "--format=%cI"); since != "" {
		query += "&since=" + url.QueryEscape(since)
	}
	history := daemonGet("/asks/history?" + query)
	if history == nil {
		return fmt.Errorf("history unavailable; leaving message unchanged")
	}
	previous := strings.Fields(gitOutput("", "log", "-1", "--format=%(trailers:key=Repowire-Thread,valueonly)"))
	gitArgs := []string{"-c", "trailer.separators=:", "interpret-trailers", "--no-divider", "--if-exists=addIfDifferent", "--if-missing=add"}
	for _, cid := range anyStrings(history["threads"]) {
		if !contains(previous, cid) && !strings.ContainsAny(cid, "\r\n") {
			gitArgs = append(gitArgs, "--trailer", "Repowire-Thread: "+cid)
		}
	}
	if session := stringValue(history, "repowire_session_id"); session != "" && !strings.ContainsAny(session, "\r\n") {
		gitArgs = append(gitArgs, "--trailer", "Repowire-Session: "+session)
	}
	if len(gitArgs) == 6 {
		return nil
	}
	cmd := exec.Command("git", gitArgs...)
	cmd.Stdin = strings.NewReader(string(original))
	updated, err := cmd.Output()
	if err != nil {
		return err
	}
	// Replace atomically so a failed write cannot truncate the user's message.
	info, err := os.Lstat(args[0])
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("message is not a regular file")
	}
	temp, err := os.CreateTemp(filepath.Dir(args[0]), ".repowire-msg-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err = temp.Chmod(info.Mode().Perm()); err == nil {
		_, err = temp.Write(updated)
	}
	closeErr := temp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(temp.Name(), args[0])
}
