package routing

// DecisionBench sanity-check adapter. DecisionBench (arXiv 2605.19099,
// huggingface.co/decisionbench) routes subtasks among 11 stateless models
// described by markdown profile cards. Its data carries no licence, so nothing
// is vendored: the caller downloads the cards and Stage-2 delegations.jsonl
// files and points this adapter at them at runtime.
//
// Mapping onto our schema:
//   - each delegation (an orchestrator handing `subtask` to `peer`) becomes one
//     explicit Example whose declared target is the orchestrator's own pick;
//   - candidates are every carded model except the orchestrator (DecisionBench
//     forbids self-delegation), with the card body as the claimed description
//     and no attested state, since the substrate has none.
//
// The label is therefore agreement with the orchestrator, the same quantity
// our static track measures against a human sender. DecisionBench's own
// dominant-skill precision label is not reconstructible from the released
// code, so it is not used. Attested-only arms see empty candidates here by
// design: the stateless substrate is exactly what they cannot exploit.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Card is one DecisionBench profile card.
type Card struct {
	Model string
	Body  string
}

// ParseCard reads a card's frontmatter `model:` and returns the body after it.
func ParseCard(md string) (Card, error) {
	rest, ok := strings.CutPrefix(strings.TrimPrefix(md, "\uFEFF"), "---\n")
	if !ok {
		return Card{}, fmt.Errorf("card has no frontmatter")
	}
	front, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return Card{}, fmt.Errorf("card frontmatter is not closed")
	}
	for _, line := range strings.Split(front, "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(key) == "model" {
			return Card{Model: strings.TrimSpace(value), Body: strings.TrimSpace(body)}, nil
		}
	}
	return Card{}, fmt.Errorf("card frontmatter has no model")
}

// LoadCards reads every *.md card in dir (one flavor: c1_human, c2_static or
// c3_llm_judge), skipping README.md and citations.md.
func LoadCards(dir string) ([]Card, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, err
	}
	var cards []Card
	for _, path := range paths {
		if base := filepath.Base(path); base == "README.md" || base == "citations.md" {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		card, err := ParseCard(string(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		cards = append(cards, card)
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].Model < cards[j].Model })
	return cards, nil
}

// delegation is one delegations.jsonl line (decision_bench/call_model.py).
type delegation struct {
	TaskID  string `json:"task_id"`
	Peer    string `json:"peer"`
	Subtask string `json:"subtask"`
	Error   string `json:"error"`
}

// DelegationExamples converts one orchestrator's delegations.jsonl into replay
// examples. Failed delegations and delegations to uncarded peers are dropped.
func DelegationExamples(r io.Reader, orchestrator string, cards []Card) ([]Example, error) {
	byModel := map[string]Card{}
	for _, c := range cards {
		byModel[c.Model] = c
	}
	candidates := make([]Candidate, 0, len(cards))
	for _, c := range cards {
		if c.Model != orchestrator {
			candidates = append(candidates, Candidate{
				PeerID: c.Model, DisplayName: c.Model, Description: clip(c.Body, maxTextBytes), Access: AccessOK,
			})
		}
	}
	var out []Example
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for n := 0; scanner.Scan(); n++ {
		var d delegation
		if err := json.Unmarshal(scanner.Bytes(), &d); err != nil {
			return nil, fmt.Errorf("delegations line %d: %w", n+1, err)
		}
		if _, carded := byModel[d.Peer]; !carded || d.Error != "" || d.Peer == orchestrator {
			continue
		}
		peer := d.Peer
		sender := Candidate{PeerID: orchestrator, DisplayName: orchestrator}
		if c, ok := byModel[orchestrator]; ok {
			sender.Description = clip(c.Body, maxTextBytes)
		}
		out = append(out, Example{
			TraceID: fmt.Sprintf("decisionbench:%s:%s:%d", orchestrator, d.TaskID, n),
			Decision: Decision{
				Schema: SchemaV1, Surface: "decisionbench", Mode: "off", Arm: ArmExplicit,
				Sender: &sender, SenderRaw: orchestrator,
				Declared:   Declared{Raw: &peer, PeerID: &peer, Resolution: ResolutionResolved},
				Chosen:     &Chosen{PeerID: peer, DisplayName: peer},
				Request:    NewRequest(d.Subtask, nil, nil, false, false, 0),
				Candidates: candidates, RegistrySize: len(cards),
			},
			Shadow: map[string]RouterResult{},
		})
	}
	return out, scanner.Err()
}
