package state

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Retention (rfcs/retention.md): one policy for everything the daemon keeps.
// Content is text a peer or the human wrote; it is stripped in place, or its
// row deleted when the row is nothing but content. Records are the rows that
// say who did what and when; they are deleted. Live state (open jobs,
// unexpired certificates, identity and schedule tables) is never touched.

// RetentionPolicy says how long each class is kept. Zero keeps forever.
type RetentionPolicy struct {
	Content time.Duration
	Records time.Duration
}

// sweepBatch bounds each statement so a first sweep over a large journal
// never holds the writer for long.
const sweepBatch = 500

// RemoteWindow is the fixed life of a cross-machine record: it is protocol
// state (a live ask is retried and answered from its row until then), so the
// policy does not move it.
const RemoteWindow = 7 * 24 * time.Hour

// Message events whose payload carries authored text, and the keys holding it.
var (
	contentEventTypes = []string{"notification", "broadcast", "ask", "query", "response", "ask_delegated"}
	transcriptTypes   = []string{"chat_turn", "chat_turn_delta"}
	eventContentKeys  = []string{"$.text", "$.message", "$.note", "$.question", "$.attachments"}
	traceContentKeys  = []string{"$.text", "$.reply", "$.note", "$.request.text"}
)

type sweepRule struct {
	name string
	// table and where select the rows; set is the UPDATE assignment, empty
	// for a delete. where must stop matching a row once set has run, so a
	// batch loop ends.
	table, where, set string
	args              []any
}

// ContentCutoff is the instant before which content goes: the records
// cutoff when records are kept for less time, zero when both are kept
// forever.
func (p RetentionPolicy) ContentCutoff(now time.Time) time.Time {
	d := p.Content
	if p.Records > 0 && (d == 0 || p.Records < d) {
		d = p.Records
	}
	if d == 0 {
		return time.Time{}
	}
	return now.Add(-d)
}

// sweepStamp is the cutoff as stored timestamps compare against it. Every
// table stores ISO-8601 UTC, written as ...Z or ...+00:00 with or without
// fractions; comparing on the whole second ignores the suffix.
func sweepStamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05") }

func jsonPresent(column string, keys []string) string {
	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = fmt.Sprintf("json_type(%s, '%s') IS NOT NULL", column, key)
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

func jsonStrip(column string, keys []string) string {
	quoted := make([]string, len(keys))
	for i, key := range keys {
		quoted[i] = "'" + key + "'"
	}
	return fmt.Sprintf("%s = json_set(json_remove(%s, %s), '$.content_expired', json('true'))", column, column, strings.Join(quoted, ", "))
}

func inList(values []string) string {
	return "('" + strings.Join(values, "', '") + "')"
}

// retentionRules lists the rules in the order a sweep runs them: records
// first, so content rules only see rows that survive.
func retentionRules(p RetentionPolicy, now time.Time) []sweepRule {
	var rules []sweepRule
	if p.Records > 0 {
		r := sweepStamp(now.Add(-p.Records))
		rules = append(rules,
			sweepRule{name: "delivery_traces.delete", table: "delivery_traces", where: "ts < ?", args: []any{r}},
			sweepRule{name: "events.delete", table: "events", where: "timestamp < ?", args: []any{r}},
			sweepRule{name: "inbox_items.delete", table: "inbox_items", where: "created_at < ?", args: []any{r}},
			sweepRule{name: "tracked_work.delete", table: "tracked_work",
				where: "state IN " + inList(terminalStateList()) + " AND COALESCE(completed_at, updated_at) < ?", args: []any{r}},
			sweepRule{name: "operations.delete", table: "operations",
				where: "completed_at IS NOT NULL AND completed_at < ?", args: []any{r}},
			// A runtime renews from its newest certificate even after it
			// expired, so each peer keeps its newest one.
			sweepRule{name: "runtime_identity_certificates.delete", table: "runtime_identity_certificates",
				where: "expires_at < ? AND EXISTS (SELECT 1 FROM runtime_identity_certificates newer " +
					"WHERE newer.peer_id = runtime_identity_certificates.peer_id AND newer.issued_at > runtime_identity_certificates.issued_at)", args: []any{r}},
			// An item's push outcomes and surface refs go with it.
			sweepRule{name: "inbox_pushes.delete", table: "inbox_pushes", where: "item_id NOT IN (SELECT id FROM inbox_items)"},
			sweepRule{name: "inbox_refs.delete", table: "inbox_refs", where: "item_id NOT IN (SELECT id FROM inbox_items)"},
		)
	}
	if cutoff := p.ContentCutoff(now); !cutoff.IsZero() {
		c := []any{sweepStamp(cutoff)}
		rules = append(rules,
			sweepRule{name: "peer_messages.delete", table: "peer_messages", where: "created_at < ?", args: c},
			sweepRule{name: "delivery_traces.strip", table: "delivery_traces",
				where: "ts < ? AND " + jsonPresent("detail_json", traceContentKeys),
				set:   jsonStrip("detail_json", traceContentKeys), args: c},
			sweepRule{name: "events.delete_transcripts", table: "events",
				where: "type IN " + inList(transcriptTypes) + " AND timestamp < ?", args: c},
			sweepRule{name: "events.strip", table: "events",
				where: "type IN " + inList(contentEventTypes) + " AND timestamp < ? AND " + jsonPresent("payload_json", eventContentKeys),
				set:   jsonStrip("payload_json", eventContentKeys), args: c},
			sweepRule{name: "inbox_items.strip", table: "inbox_items",
				where: "created_at < ? AND (text != '' OR attachments_json != '[]' OR question_json IS NOT NULL)",
				set:   "text = '', attachments_json = '[]', question_json = NULL", args: c},
		)
	}
	remote := now.Add(-RemoteWindow).Unix()
	for _, table := range []string{"remote_inbound", "remote_fences", "remote_outbound"} {
		rules = append(rules, sweepRule{name: table + ".delete", table: table, where: "created_at < ?", args: []any{remote}})
	}
	return rules
}

func terminalStateList() []string {
	out := make([]string, 0, len(terminalWorkStates))
	for state := range terminalWorkStates {
		out = append(out, state)
	}
	sort.Strings(out)
	return out
}

// Sweep applies policy as of now and returns the rows each rule changed,
// keyed by rule name such as "events.strip". A failing rule does not stop
// the others; the error returned is the first one.
func (s *Store) Sweep(ctx context.Context, p RetentionPolicy, now time.Time) (map[string]int, error) {
	counts := map[string]int{}
	var first error
	for _, rule := range retentionRules(p, now) {
		n, err := s.applyRule(ctx, rule)
		if err != nil && first == nil {
			first = fmt.Errorf("retention %s: %w", rule.name, err)
		}
		if n > 0 {
			counts[rule.name] = n
		}
	}
	return counts, first
}

func (s *Store) applyRule(ctx context.Context, rule sweepRule) (int, error) {
	batch := `rowid IN (SELECT rowid FROM ` + rule.table + ` WHERE ` + rule.where + fmt.Sprintf(` LIMIT %d)`, sweepBatch)
	query := `DELETE FROM ` + rule.table + ` WHERE ` + batch
	if rule.set != "" {
		query = `UPDATE ` + rule.table + ` SET ` + rule.set + ` WHERE ` + batch
	}
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := s.db.ExecContext(ctx, query, rule.args...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
		if n < sweepBatch {
			return total, nil
		}
	}
}
