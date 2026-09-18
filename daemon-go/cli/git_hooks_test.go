package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestInstallGitHookPreservesExistingHooks(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := installGitHook(); err != nil {
		t.Fatal(err)
	}
	if err := installGitHook(); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	path := filepath.Join(repo, ".git/hooks/prepare-commit-msg")
	original := []byte("#!/bin/sh\necho existing\n")
	if err := os.WriteFile(path, original, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installGitHook(); err == nil {
		t.Fatal("overwrote existing hook")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(original) {
		t.Fatal(string(got))
	}
	if out, err := exec.Command("git", "config", "core.hooksPath", "custom-hooks").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := installGitHook(); err == nil {
		t.Fatal("accepted custom hooksPath")
	}
	if _, err := os.Stat("custom-hooks"); !os.IsNotExist(err) {
		t.Fatal("modified custom hooks directory")
	}
}
