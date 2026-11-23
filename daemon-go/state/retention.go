package state

import (
	"context"
	"database/sql"
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

// SweepReport counts what a sweep changed (or, dry, would change), keyed by
// rule name such as "events.strip". Errors names the rules that failed.
type SweepReport struct {
	Counts map[string]int `json:"counts"`
	Errors []string       `json:"errors,omitempty"`
}

// Total is the number of rows the sweep touched.
func (r SweepReport) Total() int {
	n := 0
	for _, c := range r.Counts {
		n += c
	}
	return n
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

// Cutoffs returns the content cutoff (the later of the two when records are
// kept for less time) and the records cutoff, each zero when its class is
// kept forever.
func (p RetentionPolicy) Cutoffs(now time.Time) (content, records time.Time) {
	if p.Records > 0 {
		records = now.Add(-p.Records)
	}
	if p.Content > 0 {
		content = now.Add(-p.Content)
	}
	if !records.IsZero() && (content.IsZero() || records.After(content)) {
		content = records
	}
	return content, records
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

// retentionRules lists the rules in the order a sweep runs them. Records go
// first, and every content rule is bounded below by the records cutoff, so
// no row matches two rules: a dry run's per-rule counts are then exactly what
// a sweep changes.
func retentionRules(p RetentionPolicy, now time.Time) []sweepRule {
	var rules []sweepRule
	content, records := p.Cutoffs(now)
	liveItems := "SELECT id FROM inbox_items"
	if !records.IsZero() {
		r := sweepStamp(records)
		liveItems += " WHERE created_at >= '" + r + "'"
		rules = append(rules,
			sweepRule{name: "delivery_traces.delete", table: "delivery_traces", where: "ts < ?", args: []any{r}},
			sweepRule{name: "events.delete", table: "events", where: "timestamp < ?", args: []any{r}},
			sweepRule{name: "inbox_items.delete", table: "inbox_items", where: "created_at < ?", args: []any{r}},
			sweepRule{name: "tracked_work.delete", table: "tracked_work",
				where: "state IN " + inList(terminalStateList()) + " AND COALESCE(completed_at, updated_at) < ?", args: []any{r}},
			sweepRule{name: "operations.delete", table: "operations",
				where: "completed_at IS NOT NULL AND completed_at < ?", args: []any{r}},
			sweepRule{name: "runtime_identity_certificates.delete", table: "runtime_identity_certificates",
				where: "expires_at < ?", args: []any{r}},
		)
	}
	// An item's push outcomes and surface refs go with it.
	rules = append(rules,
		sweepRule{name: "inbox_pushes.delete", table: "inbox_pushes", where: "item_id NOT IN (" + liveItems + ")"},
		sweepRule{name: "inbox_refs.delete", table: "inbox_refs", where: "item_id NOT IN (" + liveItems + ")"},
	)
	if !content.IsZero() {
		// Peer messages are pure content and have no records rule, so their
		// rule has no lower bound: a message older than both cutoffs goes too.
		rules = append(rules, sweepRule{name: "peer_messages.delete", table: "peer_messages", where: "created_at < ?", args: []any{sweepStamp(content)}})
	}
	if !content.IsZero() && (records.IsZero() || content.After(records)) {
		// [records, content): older rows were deleted as records above.
		window := func(column string) (string, []any) {
			if records.IsZero() {
				return column + " < ?", []any{sweepStamp(content)}
			}
			return column + " < ? AND " + column + " >= ?", []any{sweepStamp(content), sweepStamp(records)}
		}
		created, createdArgs := window("created_at")
		ts, tsArgs := window("ts")
		at, atArgs := window("timestamp")
		rules = append(rules,
			sweepRule{name: "delivery_traces.strip", table: "delivery_traces",
				where: ts + " AND " + jsonPresent("detail_json", traceContentKeys),
				set:   jsonStrip("detail_json", traceContentKeys), args: tsArgs},
			sweepRule{name: "events.delete_transcripts", table: "events",
				where: "type IN " + inList(transcriptTypes) + " AND " + at, args: atArgs},
			sweepRule{name: "events.strip", table: "events",
				where: "type IN " + inList(contentEventTypes) + " AND " + at + " AND " + jsonPresent("payload_json", eventContentKeys),
				set:   jsonStrip("payload_json", eventContentKeys), args: atArgs},
			sweepRule{name: "inbox_items.strip", table: "inbox_items",
				where: created + " AND (text != '' OR attachments_json != '[]' OR question_json IS NOT NULL)",
				set:   "text = '', attachments_json = '[]', question_json = NULL", args: createdArgs},
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

// Sweep applies policy as of now. A dry run counts each rule's rows with one
// short statement per rule; the rules are disjoint, so the counts are exactly
// what a sweep would change. A failing rule is named in the report and does
// not stop the others; the error returned is the first one.
func (s *Store) Sweep(ctx context.Context, p RetentionPolicy, now time.Time, dry bool) (SweepReport, error) {
	report := SweepReport{Counts: map[string]int{}}
	var first error
	for _, rule := range retentionRules(p, now) {
		var n int
		var err error
		if dry {
			err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+rule.table+` WHERE `+rule.where, rule.args...).Scan(&n)
		} else {
			n, err = applyRule(ctx, s.db, rule)
		}
		if err != nil {
			report.Errors = append(report.Errors, rule.name)
			if first == nil {
				first = fmt.Errorf("retention %s: %w", rule.name, err)
			}
		}
		if n > 0 {
			report.Counts[rule.name] = n
		}
	}
	return report, first
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func applyRule(ctx context.Context, db execer, rule sweepRule) (int, error) {
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
		res, err := db.ExecContext(ctx, query, rule.args...)
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

// Vacuum rebuilds the database file so swept space is returned to the disk.
func (s *Store) Vacuum(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `VACUUM`)
	return err
}
