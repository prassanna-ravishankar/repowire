package service

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
)

var recallTokenRE = regexp.MustCompile(`[a-zA-Z0-9][a-zA-Z0-9_.@/-]{2,}`)

type recallHit struct {
	label, snippet string
	score          int
}

func addOrchestratorRecall(text, fromName, fromID string, target *proto.Peer, settings config.OrchestratorRecallConfig) string {
	if !settings.Enabled || target == nil || target.Role != proto.RoleOrchestrator {
		return text
	}
	query := strings.Join([]string{text, fromName, fromID, string(target.DisplayName), string(target.PeerID), target.Circle}, " ")
	tokens := map[string]bool{}
	stop := map[string]bool{"from": true, "with": true, "that": true, "this": true, "there": true, "reply": true, "ack": true, "ask": true, "message": true, "repowire": true}
	for _, token := range recallTokenRE.FindAllString(query, -1) {
		token = strings.ToLower(strings.Trim(token, "._-/@"))
		if token != "" && !stop[token] {
			tokens[token] = true
		}
	}
	if len(tokens) == 0 {
		return text
	}
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".repowire", "orchestrator")
	sources := []string{filepath.Join(root, "comms.md"), filepath.Join(root, "projects.md")}
	memory, _ := filepath.Glob(filepath.Join(root, "memory", "*.md"))
	sort.Strings(memory)
	for _, path := range memory {
		if filepath.Base(path) != "MEMORY.md" {
			sources = append(sources, path)
		}
	}
	hits := []recallHit{}
	for _, path := range sources {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if settings.MaxFileChars > 0 && len(raw) > settings.MaxFileChars {
			raw = raw[:settings.MaxFileChars]
		}
		content := string(raw)
		label, _ := filepath.Rel(root, path)
		score := 0
		lower := strings.ToLower(label + "\n" + content)
		for token := range tokens {
			count := strings.Count(lower, token)
			if count > 4 {
				count = 4
			}
			score += count
			if strings.Contains(strings.ToLower(label), token) {
				score += 2
			}
		}
		if score == 0 {
			continue
		}
		best := ""
		bestScore := 0
		for _, line := range strings.Split(content, "\n") {
			clean := strings.TrimSpace(line)
			lineScore := 0
			for token := range tokens {
				if strings.Contains(strings.ToLower(clean), token) {
					lineScore++
				}
			}
			if lineScore > bestScore || (lineScore == bestScore && lineScore > 0 && (best == "" || len(clean) < len(best))) {
				best, bestScore = clean, lineScore
			}
		}
		if best != "" {
			if len(best) > 220 {
				best = best[:219] + "…"
			}
			hits = append(hits, recallHit{label: label, snippet: best, score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score == hits[j].score {
			return hits[i].label < hits[j].label
		}
		return hits[i].score > hits[j].score
	})
	if settings.MaxHits > 0 && len(hits) > settings.MaxHits {
		hits = hits[:settings.MaxHits]
	}
	if len(hits) == 0 {
		return text
	}
	lines := []string{"[repowire recall]"}
	for _, hit := range hits {
		lines = append(lines, "- "+hit.label+": "+hit.snippet)
	}
	lines = append(lines, "[/repowire recall]")
	block := strings.Join(lines, "\n")
	if settings.MaxChars > 0 && len(block) > settings.MaxChars {
		block = block[:settings.MaxChars-1] + "…"
	}
	return block + "\n\n" + text
}
