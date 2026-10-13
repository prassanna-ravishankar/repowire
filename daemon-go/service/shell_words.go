package service

import (
	"regexp"
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

// Redaction is an allowlist: a value or positional is shown only when it
// matches a grammar that cannot carry a secret.
var (
	sessionIDShape = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|ses_[A-Za-z0-9]{8,})$`)
	modelIDShape   = regexp.MustCompile(`^(claude|gpt|o[0-9]|gemini|codex|opus|sonnet|haiku|fable)[A-Za-z0-9.-]*$`)
)

// shownValue reports whether flag's value may be printed.
func shownValue(flag, value string) bool {
	switch flag {
	case "--resume", "-r", "--session", "--session-id":
		return sessionIDShape.MatchString(value)
	case "--permission-mode":
		return claudePermissionModes[value]
	case "--sandbox", "-s":
		return codexSandboxModes[value]
	case "--ask-for-approval", "-a":
		return codexApprovalPolicies[value]
	case "--model", "-m":
		return modelIDShape.MatchString(value)
	}
	return false
}

// booleanFlags take no value, so the word after them is positional.
var booleanFlags = map[string]bool{
	"--dangerously-skip-permissions": true, "--allow-dangerously-skip-permissions": true,
	"--dangerously-bypass-approvals-and-sandbox": true, "--dangerously-bypass-hook-trust": true, "--approve-for-me": true,
	"--continue": true, "--fork-session": true, "--last": true, "--all": true, "--include-non-interactive": true,
	"--strict-config": true, "--oss": true, "--worktree": true, "--search": true, "--no-alt-screen": true, "--no-daemon": true,
	"--verbose": true, "--debug": true, "--full-auto": true, "--yolo": true,
}

// RedactCommand prints a launch line with every value redacted unless it
// matches a known grammar: env assignment values, flag values (shown only for
// session ids, listed permission modes, and model ids), and positionals
// (shown only for the `resume` subcommand and session ids). The program,
// `env`, and flag names are kept. It returns what it redacted.
func RedactCommand(command string) (string, []string) {
	words, ok := splitShellWords(command)
	if !ok {
		return "<redacted: unparseable command>", []string{"*"}
	}
	var redacted []string
	redact := func(i int, raw, name string) {
		words[i].Raw, redacted = raw, append(redacted, name)
	}
	prog := programIndex(words)
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case i < prog:
			if name, _, found := strings.Cut(w.Value, "="); found {
				redact(i, name+"=<redacted>", name)
			}
		case i == prog:
		case isFlag(w.Value):
			name, value, hasValue := strings.Cut(w.Value, "=")
			if hasValue {
				if !shownValue(name, value) {
					redact(i, name+"=<redacted>", name)
				}
				continue
			}
			if i+1 < len(words) && !isFlag(words[i+1].Value) && !booleanFlags[name] {
				if !shownValue(name, words[i+1].Value) {
					redact(i+1, "<redacted>", name)
				}
				i++
			}
		case w.Value == "resume" || sessionIDShape.MatchString(w.Value):
		default:
			redact(i, "<redacted>", "positional")
		}
	}
	return joinShellWords(words), redacted
}
