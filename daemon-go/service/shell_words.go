package service

import (
	"strings"
)

// shellWord is one word of a command line: Raw is its original text (quotes
// intact), Value its unquoted value. Rejoining untouched words by Raw keeps
// quoted whitespace and JSON arguments exactly as configured.
type shellWord struct {
	Raw, Value string
}

// splitShellWords splits a POSIX-ish command line into words. ok is false on
// unbalanced quotes.
func splitShellWords(command string) (words []shellWord, ok bool) {
	rest := strings.TrimLeft(command, " \t\n")
	for rest != "" {
		value, next, ok := firstShellWord(rest)
		if !ok {
			return nil, false
		}
		raw := strings.TrimRight(rest[:len(rest)-len(next)], " \t\n")
		words = append(words, shellWord{Raw: raw, Value: value})
		rest = strings.TrimLeft(next, " \t\n")
	}
	return words, true
}

func wordsFromValues(values []string) []shellWord {
	words := make([]shellWord, 0, len(values))
	for _, v := range values {
		words = append(words, shellWord{Raw: ShellQuote(v), Value: v})
	}
	return words
}

func joinShellWords(words []shellWord) string {
	raws := make([]string, 0, len(words))
	for _, w := range words {
		raws = append(raws, w.Raw)
	}
	return strings.Join(raws, " ")
}

// programIndex returns the index of the program word, skipping an `env`
// prefix and VAR=value assignments.
func programIndex(words []shellWord) int {
	for i, w := range words {
		if w.Value == "env" || (strings.Contains(w.Value, "=") && !strings.HasPrefix(w.Value, "-")) {
			continue
		}
		return i
	}
	return len(words)
}

func isFlag(value string) bool { return strings.HasPrefix(value, "-") && value != "-" }

func flagName(value string) string {
	name, _, _ := strings.Cut(value, "=")
	return name
}

// endOfOptions is the index of the first `--` at or after from (len(words)
// when absent). Words after it are positional, whatever they look like.
func endOfOptions(words []shellWord, from int) int {
	for i := from; i < len(words); i++ {
		if words[i].Value == "--" {
			return i
		}
	}
	return len(words)
}

// RedactShown is what RedactCommand may print besides the program, `env`,
// flag names and `--`: the resume argument the daemon itself supplied, the
// listed permission words, and model ids from the daemon's own config.
type RedactShown struct {
	ResumeID string
	Models   map[string]bool
}

// booleanFlags take no value, so the word after them is positional.
var booleanFlags = map[string]bool{
	"--dangerously-skip-permissions": true, "--allow-dangerously-skip-permissions": true,
	"--dangerously-bypass-approvals-and-sandbox": true, "--dangerously-bypass-hook-trust": true, "--approve-for-me": true,
	"--continue": true, "--fork-session": true, "--last": true, "--all": true, "--include-non-interactive": true,
	"--strict-config": true, "--oss": true, "--worktree": true, "--search": true, "--no-alt-screen": true, "--no-daemon": true,
	"--verbose": true, "--debug": true, "--full-auto": true, "--yolo": true,
}

func (s RedactShown) value(flag, value string) bool {
	switch flag {
	case "--resume", "-r", "--session":
		return s.ResumeID != "" && value == s.ResumeID
	case "--permission-mode":
		return claudePermissionModes[value]
	case "--sandbox", "-s":
		return codexSandboxModes[value]
	case "--ask-for-approval", "-a":
		return codexApprovalPolicies[value]
	case "--model", "-m":
		return s.Models[value]
	}
	return false
}

// RedactCommand prints a launch line for results and the journal. Every value
// is redacted except: the resume argument the daemon supplied (shown.ResumeID,
// after a resume flag or codex's `resume` subcommand), listed permission
// words, and a model argument equal to a model id in the daemon's config.
// Every other flag value, env value and positional (prompt text included) is
// <redacted>. This is a contract about positions, not secrecy: a secret placed
// in an allowed position (for example a prompt shaped like the session id, or
// a configured model id) is shown, so do not put secrets there.
func RedactCommand(command string, shown RedactShown) (string, []string) {
	words, ok := splitShellWords(command)
	if !ok {
		return "<redacted: unparseable command>", []string{"*"}
	}
	var redacted []string
	redact := func(i int, raw, name string) {
		words[i].Raw, redacted = raw, append(redacted, name)
	}
	prog := programIndex(words)
	end := endOfOptions(words, prog+1)
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case i < prog:
			if name, _, found := strings.Cut(w.Value, "="); found {
				redact(i, name+"=<redacted>", name)
			}
		case i == prog, i == end:
		case i > end:
			redact(i, "<redacted>", "positional")
		case isFlag(w.Value):
			name, value, hasValue := strings.Cut(w.Value, "=")
			if hasValue {
				if !shown.value(name, value) {
					redact(i, name+"=<redacted>", name)
				}
				continue
			}
			if i+1 < end && !isFlag(words[i+1].Value) && !booleanFlags[name] {
				if !shown.value(name, words[i+1].Value) {
					redact(i+1, "<redacted>", name)
				}
				i++
			}
		case w.Value == "resume" && shown.ResumeID != "" && i+1 < len(words) && words[i+1].Value == shown.ResumeID:
			i++ // the resume argument the daemon supplied
		default:
			redact(i, "<redacted>", "positional")
		}
	}
	return joinShellWords(words), redacted
}
