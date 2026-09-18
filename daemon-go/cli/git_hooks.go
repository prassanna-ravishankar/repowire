package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func installGitHook() error {
	// A custom hooksPath may be shared globally or owned by a hook manager.
	// Never overwrite it or silently change which hooks Git executes.
	if out, err := exec.Command("git", "config", "--get", "core.hooksPath").Output(); err == nil {
		return fmt.Errorf("core.hooksPath is configured (%s); add `repowire hook prepare-commit-msg \"$@\"` to your existing prepare-commit-msg hook", strings.TrimSpace(string(out)))
	}
	out, err := exec.Command("git", "rev-parse", "--git-path", "hooks/prepare-commit-msg").Output()
	if err != nil {
		return fmt.Errorf("run setup --git-hooks inside a Git repository: %w", err)
	}
	path := strings.TrimSpace(string(out))
	// Single quotes are shell-safe even for executable paths with $ or backticks.
	quoted := "'" + strings.ReplaceAll(executable(), "'", "'\"'\"'") + "'"
	content := "#!/bin/sh\n# Repowire prepare-commit-msg\n" + quoted + " hook prepare-commit-msg \"$@\" || echo 'repowire: git trailers unavailable' >&2\nexit 0\n"
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if os.IsExist(err) {
		return fmt.Errorf("prepare-commit-msg already exists; add `repowire hook prepare-commit-msg \"$@\"` to it (existing hook left unchanged)")
	}
	if err != nil {
		return err
	}
	_, err = file.WriteString(content)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
