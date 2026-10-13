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

// simpleWord matches ids, subcommands and paths: words that cannot carry a
// structured secret such as JSON or a quoted prompt.
var simpleWord = regexp.MustCompile(`^[A-Za-z0-9._:/@%+-]+$`)

// redactSafeFlags are the flags whose values a printed restart command keeps.
var redactSafeFlags = map[string]bool{
	"--resume": true, "-r": true, "--session": true, "--permission-mode": true, "--model": true, "-m": true,
	"--sandbox": true, "-s": true, "--ask-for-approval": true, "-a": true,
}

// RedactCommand replaces argument values in a launch line with <redacted>:
// env assignments, every flag value except redactSafeFlags, and positional
// words that are not simple (JSON, quoted prompts). It keeps the program, flag
// names, subcommands and ids, and returns what it redacted.
func RedactCommand(command string) (string, []string) {
	words, ok := splitShellWords(command)
	if !ok {
		return "<redacted: unparseable command>", []string{"*"}
	}
	var redacted []string
	prog := programIndex(words)
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case i < prog:
			if name, _, found := strings.Cut(w.Value, "="); found {
				words[i].Raw, redacted = name+"=<redacted>", append(redacted, name)
			}
		case i == prog:
		case isFlag(w.Value):
			name, _, hasValue := strings.Cut(w.Value, "=")
			if hasValue {
				if !redactSafeFlags[name] {
					words[i].Raw, redacted = name+"=<redacted>", append(redacted, name)
				}
				continue
			}
			if i+1 < len(words) && !isFlag(words[i+1].Value) && !booleanFlags[name] {
				if !redactSafeFlags[name] {
					words[i+1].Raw, redacted = "<redacted>", append(redacted, name)
				}
				i++
			}
		case !simpleWord.MatchString(w.Value):
			words[i].Raw, redacted = "<redacted>", append(redacted, "positional")
		}
	}
	return joinShellWords(words), redacted
}

// booleanFlags take no value, so the word after them is positional.
var booleanFlags = map[string]bool{
	"--dangerously-skip-permissions": true, "--allow-dangerously-skip-permissions": true,
	"--dangerously-bypass-approvals-and-sandbox": true, "--yolo": true, "--full-auto": true,
	"--continue": true, "--verbose": true, "--debug": true, "--last": true, "--all": true,
}
