package proto

import (
	"errors"
	"strings"
	"testing"
)

func TestProvenanceNormalizedTreatsZeroAsUnknownAddressable(t *testing.T) {
	got := Provenance{}.Normalized()
	if got.Source != SourceUnknown || !got.Addressable {
		t.Fatalf("zero value normalized to %+v, want unknown/addressable", got)
	}
	if got := (Provenance{Source: "desktop", Addressable: false}).Normalized(); got.Source != SourceUnknown || got.Addressable {
		t.Fatalf("invalid source normalized to %+v, want unknown with addressable preserved", got)
	}
}

func TestProvenanceMergePreservesKnownWhenUpdateMissing(t *testing.T) {
	stored := Provenance{Source: SourceCodexAppServer, ParentRuntimeID: "parent", Addressable: false, AddressableReason: "subagent_direct_input_denied"}
	if got := stored.Merge(nil); got != stored {
		t.Fatalf("nil update changed provenance: %+v", got)
	}
	fresh := Provenance{Source: SourceCodexAppServer, ParentRuntimeID: "parent", Ephemeral: true, Addressable: true}
	if got := stored.Merge(&fresh); got != fresh {
		t.Fatalf("explicit update did not win: %+v", got)
	}
}

func TestProvenanceInferSourceOnlyFillsUnknown(t *testing.T) {
	bridge := Provenance{}.InferSource(map[string]any{"transport": "codex-app-server"}, false)
	if bridge.Source != SourceCodexAppServer || !bridge.Addressable {
		t.Fatalf("bridge inference = %+v", bridge)
	}
	if got := (Provenance{}).InferSource(nil, true); got.Source != SourceHook {
		t.Fatalf("hook inference = %+v", got)
	}
	if got := (Provenance{}).InferSource(nil, false); got.Source != SourceUnknown {
		t.Fatalf("no evidence should stay unknown, got %+v", got)
	}
	// Ephemeral + child + denied must survive inference untouched, and a
	// known source is never re-inferred.
	child := Provenance{Source: SourceCodexAppServer, ParentRuntimeID: "p", Ephemeral: true, Addressable: false, AddressableReason: "x"}
	if got := child.InferSource(map[string]any{"transport": "hook"}, true); got != child {
		t.Fatalf("inference altered a classified peer: %+v", got)
	}
}

func TestRequireAddressableIsInboundOnly(t *testing.T) {
	child := &Peer{PeerID: "repow-1-child", DisplayName: "app-2-codex", Provenance: Provenance{Source: SourceCodexAppServer, ParentRuntimeID: "thread-parent", Addressable: false, AddressableReason: "subagent_direct_input_denied"}}
	other := &Peer{PeerID: "repow-1-other", DisplayName: "app-codex", Provenance: Provenance{Source: SourceHook, Addressable: true}}
	legacy := &Peer{PeerID: "repow-1-legacy", DisplayName: "old"} // zero provenance: addressable

	err := RequireAddressable(other, child)
	var na *NotAddressableError
	if !errors.As(err, &na) || na.PeerID != child.PeerID || na.Reason != "subagent_direct_input_denied" || na.ParentRuntimeID != "thread-parent" {
		t.Fatalf("inbound to child = %v", err)
	}
	if !strings.Contains(err.Error(), "peer_not_addressable") || !strings.Contains(err.Error(), "app-2-codex") {
		t.Fatalf("error text = %q", err.Error())
	}
	if err := RequireAddressable(nil, child); err == nil {
		t.Fatal("unknown sender must still be gated")
	}
	if err := RequireAddressable(child, other); err != nil {
		t.Fatalf("child outbound blocked: %v", err)
	}
	if err := RequireAddressable(child, child); err != nil {
		t.Fatalf("self-target blocked: %v", err)
	}
	if err := RequireAddressable(other, legacy); err != nil {
		t.Fatalf("legacy peer gated: %v", err)
	}
}
