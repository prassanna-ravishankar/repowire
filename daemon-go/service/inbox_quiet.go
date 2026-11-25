package service

// inbox_quiet.go is the human inbox's quiet hours (#476). A surface that
// declared CapInboxHold and configured human.surfaces.<name>.quiet_hours is
// not pushed during its window: each item it would have received gets a push
// record with status held instead, durable across restarts, and the surface
// is told (surface_hold) when the window ends. Release is lazy, with no timer
// in the daemon: the surface's own client sends surface_release at that time,
// and the next Store and a reconnect release too. A release claims each held
// record (held → releasing) so racing triggers push it once, skips items the
// human has already handled, and with digest on delivers the held items that
// are not asks as one message.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// window is target's quiet window. Only a peer surface that declared
// CapInboxHold has one: a surface that cannot be told when to come back is
// never held, so nothing waits without a way out.
func (i *Inbox) window(target surfaceTarget) (config.QuietWindow, bool) {
	if target.peer == nil || !proto.HasCapability(target.peer.Metadata, proto.CapInboxHold) {
		return config.QuietWindow{}, false
	}
	return i.cfg.QuietWindow(target.name)
}

// blockingQuestion reports whether item carries a question with a deadline:
// holding it would let the deadline decide. Read from the question itself,
// so a scheduled, delegated or relayed blocking question counts too.
func blockingQuestion(item *state.InboxItem) bool {
	if item.Question == nil {
		return false
	}
	blocking, _ := item.Question["blocking"].(bool)
	scope, _ := item.Question["scope"].(string)
	return blocking || scope == "tool_permission"
}

// holds reports whether item waits for target's quiet window to end.
func (i *Inbox) holds(item *state.InboxItem, target surfaceTarget, now time.Time) bool {
	w, ok := i.window(target)
	if !ok || !w.Quiet(now) {
		return false
	}
	switch {
	case blockingQuestion(item) && w.Bypasses(config.BypassBlocking):
		return false
	case item.Kind == InboxAsk && w.Bypasses(config.BypassAsk):
		return false
	}
	return true
}

// hold records item as held for target and tells the surface when to come
// back. The caller claimed the push; hold releases the claim.
func (i *Inbox) hold(item *state.InboxItem, target surfaceTarget, now time.Time) {
	defer i.release(item.ID, target.name)
	ctx := i.baseCtx
	if err := i.store.RecordInboxPush(ctx, item.ID, state.InboxPush{Surface: target.name, Status: state.PushHeld}); err != nil {
		// A hold that cannot be recorded must not lose the push.
		log.Printf("inbox: hold %s for %s: %v; pushing it now", item.ID, target.name, err)
		i.recordPush(item, target.name, i.deliver(item, target))
		return
	}
	i.setMayHold(target.name, true)
	w, _ := i.window(target)
	i.trace(ctx, item, "surface_held", "", map[string]any{"item_id": item.ID, "surface": target.name, "release_at": w.End(now).Format(time.RFC3339)})
	i.announceHold(ctx, target, now)
	i.SyncPresence(ctx)
}

// announceHold tells target how many items wait and when its window ends,
// as an absolute UTC instant computed in the window's zone.
func (i *Inbox) announceHold(ctx context.Context, target surfaceTarget, now time.Time) {
	w, ok := i.window(target)
	if !ok || !w.Quiet(now) {
		return
	}
	n, err := i.store.HeldInboxCount(ctx, target.name)
	if err != nil || n == 0 {
		return
	}
	if err := i.transport.Send(ctx, target.peer.PeerID, map[string]any{
		"type": string(proto.FrameSurfaceHold), "held": n, "release_at": w.End(now).Format(time.RFC3339),
	}); err != nil {
		log.Printf("inbox: surface_hold to %s: %v", target.name, err)
	}
}

// SurfaceRelease answers a surface's surface_release, and runs on its
// reconnect. The surface is not trusted: in its window (clock skew, an early
// timer, a stale release_at) nothing is released and the hold is announced
// again with the current end; outside it the backlog is released. Repeating
// it is harmless.
func (i *Inbox) SurfaceRelease(p *proto.Peer) {
	human, _ := i.Human()
	if p == nil || p.PeerID == human || !proto.IsInboxSurface(p) {
		return
	}
	target := surfaceTarget{name: string(p.DisplayName), peer: p}
	now := i.now()
	i.spawn(func() {
		if w, ok := i.window(target); ok && w.Quiet(now) {
			i.announceHold(i.baseCtx, target, now)
			return
		}
		i.releaseHeld(target)
	})
}

// releaseIfDue releases target's backlog when it may have one and is not in
// its window.
func (i *Inbox) releaseIfDue(target surfaceTarget, now time.Time) {
	if !i.mayHoldFor(target.name) || !i.retryDue(target.name, now) {
		return
	}
	if w, ok := i.window(target); ok && w.Quiet(now) {
		return
	}
	i.releaseHeld(target)
}

// releaseHeld pushes target's held items, oldest first. Each is claimed
// first (held → releasing), so a racing trigger skips it; one the human has
// already handled is recorded skipped instead of pushed.
func (i *Inbox) releaseHeld(target surfaceTarget) {
	lock := i.releaseLock(target.name)
	lock.Lock()
	defer lock.Unlock()
	ctx := i.baseCtx
	items, err := i.store.HeldInboxItems(ctx, target.name)
	if err != nil {
		log.Printf("inbox: held items for %s: %v", target.name, err)
		return
	}
	var individual, digest []state.InboxItem
	for _, item := range items {
		claimed, err := i.store.ClaimHeldInboxPush(ctx, item.ID, target.name)
		if err != nil || !claimed {
			continue
		}
		if i.handled(&item) {
			if err := i.store.RecordInboxPush(ctx, item.ID, state.InboxPush{Surface: target.name, Status: state.PushSkipped}); err != nil {
				log.Printf("inbox: skip %s on %s: %v", item.ID, target.name, err)
			}
			i.trace(ctx, &item, "surface_released", "", map[string]any{"item_id": item.ID, "surface": target.name, "outcome": "skipped"})
			continue
		}
		if i.cfg.Digests(target.name) && item.Kind != InboxAsk {
			digest = append(digest, item)
		} else {
			individual = append(individual, item)
		}
	}
	if len(digest) == 1 {
		individual, digest = append(digest, individual...), nil
	}
	failed := 0
	if len(digest) > 0 && !i.pushDigest(target, digest) {
		failed++
	}
	for n := range individual {
		item := individual[n]
		i.claim(item.ID, target.name)
		out := i.deliver(&item, target)
		i.release(item.ID, target.name)
		if !i.recordRelease(&item, target.name, out, "") {
			failed++
		}
	}
	if n, err := i.store.HeldInboxCount(ctx, target.name); err == nil {
		i.setMayHold(target.name, n > 0)
	}
	i.settleRetry(target.name, failed > 0)
	i.SyncPresence(ctx)
}

// handled reports whether the human already dealt with item: read or
// replied on any surface, or an ask that is no longer theirs to answer.
func (i *Inbox) handled(item *state.InboxItem) bool {
	return item.ReadAt != "" || item.RepliedAt != "" || (item.Kind == InboxAsk && !i.Answerable(item))
}

// pushDigest delivers items as one message and records each as pushed by it.
// The digest is a rendering, not an inbox item, and its refs map to no item:
// a reply to it threads to nothing, so it says to reply on the item.
func (i *Inbox) pushDigest(target surfaceTarget, items []state.InboxItem) bool {
	var b strings.Builder
	fmt.Fprintf(&b, "%d messages arrived during quiet hours:\n", len(items))
	for _, item := range items {
		fmt.Fprintf(&b, "• @%s (%s): %s\n", strings.TrimPrefix(item.FromName, "@"), item.Kind, MessagePreview(item.Text))
	}
	b.WriteString("\nThe full messages are in the inbox. Replies to this summary are not threaded; reply on an item to answer it.")
	human, humanName := i.Human()
	summary := &state.InboxItem{
		ID: "digest-" + uuid.NewString()[:8], Direction: state.InboxIn, Kind: InboxNotify,
		FromName: "repowire:inbox", ToPeerID: string(human), ToName: string(humanName), Text: b.String(),
		CreatedAt: i.now().UTC().Format(time.RFC3339Nano),
	}
	out := i.deliver(summary, target)
	out.refs = nil
	for n := range items {
		i.recordRelease(&items[n], target.name, out, summary.ID)
	}
	return out.err == nil
}

// recordRelease records a released item's push. A release that failed
// (the bridge went away, a full queue) returns the item to held, so the next
// trigger tries again: a failure must not strand an item the human never
// saw. digestID names the summary it went in, if any.
func (i *Inbox) recordRelease(item *state.InboxItem, surface string, out pushOutcome, digestID string) bool {
	detail := map[string]any{"item_id": item.ID, "surface": surface, "outcome": "pushed"}
	if digestID != "" {
		detail["outcome"], detail["digest_id"] = "digest", digestID
	}
	if out.err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := i.store.RecordInboxPush(ctx, item.ID, state.InboxPush{Surface: surface, Status: state.PushHeld, ErrorClass: out.errorClass}); err != nil {
			log.Printf("inbox: return %s to held on %s: %v", item.ID, surface, err)
		}
		i.setMayHold(surface, true)
		detail["outcome"], detail["error_class"] = "held_again", out.errorClass
		i.trace(ctx, item, "surface_released", "fail", detail)
		return false
	}
	i.trace(i.baseCtx, item, "surface_released", "", detail)
	i.recordPush(item, surface, out)
	return true
}

// releaseBackoff spaces the Store trigger's retries of a failing release,
// so a bridge that keeps failing is not sent its whole backlog on every new
// item. A reconnect or surface_release retries at once: that is when a
// retry is likely to work. Checked on those requests; no timer.
type releaseBackoff struct {
	after time.Time
	wait  time.Duration
}

const (
	releaseRetryFirst = time.Minute
	releaseRetryMax   = 15 * time.Minute
)

func (i *Inbox) retryDue(surface string, now time.Time) bool {
	i.holdMu.Lock()
	defer i.holdMu.Unlock()
	return !now.Before(i.retry[surface].after)
}

// settleRetry doubles the surface's backoff after a release with failures
// (up to releaseRetryMax) and clears it after a clean one.
func (i *Inbox) settleRetry(surface string, failed bool) {
	i.holdMu.Lock()
	defer i.holdMu.Unlock()
	if !failed {
		delete(i.retry, surface)
		return
	}
	b := i.retry[surface]
	b.wait = min(max(b.wait*2, releaseRetryFirst), releaseRetryMax)
	b.after = i.now().Add(b.wait)
	i.retry[surface] = b
}

// releaseLock is surface's release mutex: one release at a time per surface,
// so its held items arrive oldest first even when triggers race.
func (i *Inbox) releaseLock(surface string) *sync.Mutex {
	i.holdMu.Lock()
	defer i.holdMu.Unlock()
	l := i.releasing[surface]
	if l == nil {
		l = &sync.Mutex{}
		i.releasing[surface] = l
	}
	return l
}

func (i *Inbox) setMayHold(surface string, may bool) {
	i.holdMu.Lock()
	defer i.holdMu.Unlock()
	i.mayHold[surface] = may
	i.cleared[surface] = !may
}

// mayHoldFor reports whether surface may have held items. Until a release
// has looked, any surface may: the store can hold items from before a
// restart.
func (i *Inbox) mayHoldFor(surface string) bool {
	i.holdMu.Lock()
	defer i.holdMu.Unlock()
	if !i.cleared[surface] {
		return true
	}
	return i.mayHold[surface]
}

// RecoverReleases returns pushes a stopped daemon left releasing to held,
// so the next release pushes them. Run once at startup.
func (i *Inbox) RecoverReleases(ctx context.Context) {
	if n, err := i.store.ResetReleasingInboxPushes(ctx); err != nil {
		log.Printf("inbox: recover releases: %v", err)
	} else if n > 0 {
		log.Printf("inbox: %d push(es) were mid-release at the last stop; held again", n)
	}
}

// WithClock replaces the clock quiet hours read (tests).
func (i *Inbox) WithClock(now func() time.Time) *Inbox {
	i.now = now
	return i
}
