package state

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/repowire/repowire/daemon-go/peer"
)

// Retention (rfcs/retention.md): one policy for everything the daemon keeps.
// Content is text a peer or the human wrote; it is stripped in place, or its
// row deleted when the row is nothing but content. Records are the rows that
// say who did what and when; they are deleted. Live state (open jobs, open
// asks, a runtime's newest certificate, identity and schedule tables) is
// never touched.
//
// Each table declares its retention beside its own queries (a
// tableRetention in its domain file), and the store runs the sweep: once
// when StartRetention is called and then, at most hourly, after an event
// write that finds one due. TestEveryTableDeclaresRetention fails on a table
// without a declaration.

// RetentionPolicy says how long each class is kept. Zero keeps forever.
// AskLife is how long an ask may stay open; the inbox keeps an unreplied
// ask's item that long whatever the policy (zero protects nothing).
type RetentionPolicy struct {
	Content time.Duration
	Records time.Duration
	AskLife time.Duration
}

// sweepBatch bounds each statement so a first sweep over a large journal
// never holds the writer for long.
const sweepBatch = 500

// retentionInterval is the least time between two sweeps.
const retentionInterval = time.Hour

// RemoteWindow is the fixed life of a cross-machine record: it is protocol
// state (a live ask is retried and answered from its row until then), so the
// policy does not move it.
const RemoteWindow = 7 * 24 * time.Hour

type sweepRule struct {
	name string
	// table and where select the rows; set is the UPDATE assignment, empty
	// for a delete. where must stop matching a row once set has run, so a
	// batch loop ends.
	table, where, set string
	args              []any
}

// retentionCutoffs is a policy as of one sweep, in stored-timestamp form.
// An empty records or content cutoff means that class is kept forever.
type retentionCutoffs struct {
	records, content string
	asksSince        string // an ask created at or after this may be open; "" protects none
	remote           int64  // unix seconds: the cross-machine window
}

// tableRetention is a domain's declaration for the tables it owns: the
// rules it runs per class, or why the tables are exempt. Records rules of
// every table run before any content rule, so content rules only see rows
// that survive.
type tableRetention struct {
	tables []string
	rules  func(c retentionCutoffs) (records, content []sweepRule)
	exempt string
}

// tableRetentions gathers every domain's declaration.
var tableRetentions = []tableRetention{
	deliveryTraceRetention, eventRetention, inboxRetention, workRetention, operationRetention,
	certificateRetention, peerMessageRetention, remoteRetention,
	peerMessageDeliveryRetention, daemonMetaRetention, scheduleRetention, calendarRetention,
	sessionBindingRetention, queuedDeliveryRetention, identityRetention, migrationRetention, legacyImportRetention,
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

func (p RetentionPolicy) cutoffs(now time.Time) retentionCutoffs {
	c := retentionCutoffs{remote: now.Add(-RemoteWindow).Unix()}
	if p.Records > 0 {
		c.records = sweepStamp(now.Add(-p.Records))
	}
	if cutoff := p.ContentCutoff(now); !cutoff.IsZero() {
		c.content = sweepStamp(cutoff)
	}
	if p.AskLife > 0 {
		c.asksSince = sweepStamp(now.Add(-p.AskLife))
	}
	return c
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

// retentionRules lists the rules in the order a sweep runs them: every
// table's records rules, then every table's content rules.
func retentionRules(p RetentionPolicy, now time.Time) []sweepRule {
	c := p.cutoffs(now)
	var records, content []sweepRule
	for _, t := range tableRetentions {
		if t.rules == nil {
			continue
		}
		r, k := t.rules(c)
		records, content = append(records, r...), append(content, k...)
	}
	return append(records, content...)
}

// Sweep applies policy as of now and returns the rows each rule changed,
// keyed by rule name such as "events.strip". A failing rule does not stop
// the others; the error returned is the first one. A cancelled ctx stops
// the sweep between batches.
func (s *Store) Sweep(ctx context.Context, p RetentionPolicy, now time.Time) (map[string]int, error) {
	counts := map[string]int{}
	var first error
	for _, rule := range retentionRules(p, now) {
		if err := ctx.Err(); err != nil {
			return counts, err
		}
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

// retention is the store's sweep lifecycle: started by StartRetention,
// stopped by Close.
type retention struct {
	policy RetentionPolicy
	now    func() time.Time
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	running bool
	stopped bool
	due     time.Time // no sweep before this

	beforeSweep func(ctx context.Context) // test seam: runs as a sweep starts
}

// StartRetention applies policy from now on: a sweep starts at once, and
// later ones start after an event write once an hour has passed. Sweeps run
// off the caller's path, one at a time; Close stops one in flight and waits
// for it. Call it once, while wiring.
func (s *Store) StartRetention(policy RetentionPolicy) {
	s.startRetention(&retention{policy: policy, now: time.Now})
}

func (s *Store) startRetention(r *retention) {
	r.ctx, r.cancel = context.WithCancel(context.Background())
	s.retention.Store(r)
	s.maybeSweep()
}

// maybeSweep starts a due sweep. Cheap otherwise: every event write calls it.
func (s *Store) maybeSweep() {
	r := s.retention.Load()
	if r == nil {
		return
	}
	r.mu.Lock()
	now := r.now()
	if r.stopped || r.running || now.Before(r.due) {
		r.mu.Unlock()
		return
	}
	r.running, r.due = true, now.Add(retentionInterval)
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			r.running = false
			r.mu.Unlock()
		}()
		if r.beforeSweep != nil {
			r.beforeSweep(r.ctx)
		}
		counts, err := s.Sweep(r.ctx, r.policy, now)
		if err != nil && r.ctx.Err() == nil {
			log.Printf("retention: %v", err)
		}
		if len(counts) == 0 || r.ctx.Err() != nil {
			return
		}
		log.Printf("retention: swept %v", counts)
		if err := s.AppendEvent(r.ctx, peer.Event{Type: "retention_swept", Payload: map[string]any{"counts": counts}}); err != nil {
			log.Printf("retention: journal: %v", err)
		}
	}()
}

// stopRetention cancels a sweep in flight and waits for it; no sweep starts
// afterwards.
func (s *Store) stopRetention() {
	r := s.retention.Load()
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	r.cancel()
	r.wg.Wait()
}
