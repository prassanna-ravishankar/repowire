package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/proto"
)

type fakeWireVisibility struct {
	modes map[string]string
	err   error
	loads int
}

func (f *fakeWireVisibility) SetPeerWireVisibility(_ context.Context, id, mode string) error {
	if f.modes == nil {
		f.modes = map[string]string{}
	}
	f.modes[id] = mode
	return nil
}

func (f *fakeWireVisibility) PeerWireVisibilities(context.Context) (map[string]string, error) {
	f.loads++
	out := map[string]string{}
	for k, v := range f.modes {
		out[k] = v
	}
	return out, f.err
}

// provenance is a recorded party as a stage detail holds it after a JSON
// round trip through the ledger.
func provenance(recs ...WireProvenance) map[string]any {
	out := make([]any, 0, len(recs))
	for _, r := range recs {
		m := map[string]any{"peer_id": string(r.PeerID), "circle": r.Circle, "role": string(r.Role)}
		if r.Name != "" {
			m["display_name"] = string(r.Name)
		}
		if r.Private {
			m["private"] = true
		}
		if r.Unknown {
			m["unknown"] = true
		}
		out = append(out, m)
	}
	return map[string]any{wireProvenanceKey: out}
}

var (
	recA = WireProvenance{PeerID: "repow-a", Circle: "dev", Role: proto.RoleAgent}
	recB = WireProvenance{PeerID: "repow-b", Circle: "dev", Role: proto.RoleAgent}
	recC = WireProvenance{PeerID: "repow-c", Circle: "ops", Role: proto.RoleAgent}
)

func TestWireProvenanceRecordsPartiesAsTheyAreNow(t *testing.T) {
	ctx := context.Background()
	w, _ := newTestWire(&fakeWireLedger{}, testPeers())
	if err := w.SetVisibility(ctx, "repow-b", WirePrivate); err != nil {
		t.Fatal(err)
	}
	got := w.Provenance(ctx, "repow-a", "repow-b", "", "repow-gone")
	named := recA
	named.Name = "alpha"
	want := []WireProvenance{named, {PeerID: "repow-b", Name: "beta", Circle: "dev", Role: proto.RoleAgent, Private: true}, {PeerID: "repow-gone", Unknown: true}}
	if len(got) != len(want) {
		t.Fatalf("provenance = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("provenance[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if (*Wire)(nil).Provenance(ctx, "repow-a") != nil {
		t.Fatal("a nil wire recorded provenance")
	}
}

func TestWireVisibilityPersistsAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := &fakeWireVisibility{modes: map[string]string{"repow-a": WirePrivate}}
	w := NewWire(NewAskTracker(time.Hour), &fakeWireLedger{}, testPeers(), store, config.Defaults().Wire.Stuck)
	if !w.Private(ctx, "repow-a") || w.Private(ctx, "repow-b") {
		t.Fatal("loaded visibility not applied")
	}
	if err := w.SetVisibility(ctx, "repow-a", "hidden"); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if err := w.SetVisibility(ctx, "repow-a", WireVisible); err != nil || w.Private(ctx, "repow-a") || store.modes["repow-a"] != WireVisible {
		t.Fatalf("set visible: %v, stored %q", err, store.modes["repow-a"])
	}
	if store.loads != 1 {
		t.Fatalf("loads = %d, want the table read once", store.loads)
	}

	broken := NewWire(NewAskTracker(time.Hour), &fakeWireLedger{}, testPeers(), &fakeWireVisibility{err: errors.New("disk")}, config.Defaults().Wire.Stuck)
	if !broken.Private(ctx, "repow-a") {
		t.Fatal("an unreadable visibility table must count peers as private")
	}
	if NewWire(NewAskTracker(time.Hour), &fakeWireLedger{}, testPeers(), nil, config.Defaults().Wire.Stuck).SetVisibility(ctx, "repow-a", WirePrivate) == nil {
		t.Fatal("a wire without a store accepted a visibility")
	}
}

func TestWireProtectionFollowsRecordedProvenance(t *testing.T) {
	ctx := context.Background()
	ledger := &fakeWireLedger{}
	at := wireNow.Add(-time.Hour)
	ask := func(id string, created map[string]any, hops ...map[string]any) {
		ledger.add(id, "ask", "created", "ok", "repow-b", "repow-a", at, created)
		for i, hop := range hops {
			ledger.add(id, "ask", "delegated", "ok", "repow-c", "repow-a", at.Add(time.Duration(i+1)*time.Minute), hop)
		}
	}
	ask("clean", provenance(recA, recB), provenance(recB, recC))
	ask("legacy", map[string]any{"text": "before provenance"})
	ask("legacy-hop", provenance(recA, recB), map[string]any{"note": "before provenance"})
	ask("was-private", provenance(recA, WireProvenance{PeerID: "repow-b", Circle: "dev", Role: proto.RoleAgent, Private: true}))
	ask("was-unknown", provenance(recA, WireProvenance{PeerID: "repow-b", Unknown: true}))
	ask("now-private", provenance(recA, recB))
	ledger.add("notif-1", "notify", "created", "", "", "repow-a", at, nil)
	ledger.add("notif-1", "notify", "resolved_peer", "", "repow-b", "repow-a", at, provenance(recA, recB))
	ledger.add("notif-old", "notify", "created", "", "", "repow-a", at, nil)
	ledger.add("notif-old", "notify", "resolved_peer", "", "repow-b", "repow-a", at, nil)
	w, _ := newTestWire(ledger, testPeers())

	for id, want := range map[string]string{
		"clean": "", "legacy": "no_provenance", "legacy-hop": "no_provenance",
		"was-private": WirePrivate, "was-unknown": "unknown_party", "notif-1": "", "notif-old": "no_provenance",
	} {
		got, _, err := w.Thread(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.ProtectedBy != want || got.Protected != (want != "") {
			t.Errorf("%s protected_by = %q (%v), want %q", id, got.ProtectedBy, got.Protected, want)
		}
	}
	if clean, _, _ := w.Thread(ctx, "clean"); len(clean.Provenance) != 4 || clean.Provenance[3] != recC {
		t.Fatalf("clean provenance = %+v", clean.Provenance)
	}

	// Choosing private protects the peer's earlier threads too.
	if err := w.SetVisibility(ctx, "repow-b", WirePrivate); err != nil {
		t.Fatal(err)
	}
	got, _, _ := w.Thread(ctx, "now-private")
	if got.ProtectedBy != WirePrivate || !got.Holder.Private {
		t.Fatalf("now-private = %q, holder %+v", got.ProtectedBy, got.Holder)
	}
}

// A party the registry no longer knows is shown by the last name its thread
// recorded: from provenance, or for a thread recorded before provenance, from
// a delegation's from and to (#577). With neither, its peer id.
func TestWireNamesAGonePartyByItsRecordedName(t *testing.T) {
	ctx := context.Background()
	ledger := &fakeWireLedger{}
	at := wireNow.Add(-time.Hour)
	gone := recA
	gone.PeerID, gone.Name = "repow-old-asker", "old-asker"
	ledger.add("ask-gone", "ask", "created", "ok", "repow-b", "repow-old-asker", at, provenance(gone, recB))
	ledger.add("ask-gone", "ask", "delegated", "ok", "repow-old-holder", "", at.Add(time.Minute), map[string]any{
		"from_peer_id": "repow-b", "from": "beta", "to_peer_id": "repow-old-holder", "to": "old-holder",
		wireProvenanceKey: provenance(recB, WireProvenance{PeerID: "repow-old-holder", Circle: "dev", Role: proto.RoleAgent})[wireProvenanceKey],
	})
	ledger.add("ask-bare", "ask", "created", "ok", "repow-b", "repow-nameless", at, nil)
	w, _ := newTestWire(ledger, testPeers())

	th, ok, err := w.Thread(ctx, "ask-gone")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if th.From.Known || th.From.Name != "old-asker" || th.Holder.Name != "old-holder" || th.Via[0].Name != "beta" {
		t.Fatalf("parties: from %+v holder %+v via %+v", th.From, th.Holder, th.Via)
	}
	if th.Stages[1].Peer.Name != "old-holder" {
		t.Fatalf("stage peer %+v", th.Stages[1].Peer)
	}
	if text := th.Text(0); !strings.Contains(text, "@old-asker(gone)") || strings.Contains(text, "repow-old") {
		t.Fatalf("text:\n%s", text)
	}
	bare, _, _ := w.Thread(ctx, "ask-bare")
	if bare.From.Name != "repow-nameless" {
		t.Fatalf("no recorded name: %+v", bare.From)
	}
}
