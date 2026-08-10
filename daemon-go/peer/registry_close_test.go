package peer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
)

// TestRegistryClose_JoinsTrackedGoroutine proves Close blocks until a
// goroutine spawned via spawnTracked finishes, and returns promptly once it
// does — a stub join that doesn't actually wait would pass a naive test but
// let a goroutine race store.Close() in production.
func TestRegistryClose_JoinsTrackedGoroutine(t *testing.T) {
	r, _ := newRegistry(t)

	release := make(chan struct{})
	started := make(chan struct{})
	r.spawnTracked(func() {
		close(started)
		<-release
	})
	<-started

	done := make(chan struct{})
	go func() {
		r.Close()
		close(done)
	}()

	select {
	case <-done:
		t.Fatalf("Close returned before the tracked goroutine finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Close did not return promptly after the tracked goroutine finished")
	}
}

// TestRegistryClose_NoSpawnAfterClose proves spawnTracked (and its
// LazyRepairAsync wrapper) is a no-op once Close has run. Without the
// closed-gate, a post-Close spawnTracked call either panics (wg.Add after
// Wait returned) or races a store the caller is about to close.
func TestRegistryClose_NoSpawnAfterClose(t *testing.T) {
	r, _ := newRegistry(t)
	r.Close()

	var count int32
	r.spawnTracked(func() { atomic.AddInt32(&count, 1) })
	r.LazyRepairAsync(context.Background()) // must not panic post-Close either

	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&count); got != 0 {
		t.Fatalf("spawnTracked ran a goroutine after Close, count=%d", got)
	}
}

func TestRegistryClose_FlushesDirtyMappings(t *testing.T) {
	r, store := newRegistry(t)
	id, _, err := r.AllocateAndRegister(context.Background(), AllocateParams{
		Circle: "alpha", Backend: proto.AgentClaudeCode, Machine: "m", Role: proto.RoleAgent,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	store.mu.Lock()
	before := len(store.mappings)
	store.mu.Unlock()
	if before != 0 {
		t.Fatalf("registration persisted eagerly, got %d mappings", before)
	}

	r.Close()
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.mappings[id]; !ok {
		t.Fatal("Close did not flush the dirty mapping")
	}
}

func TestPersistMappings_SkipsCleanRegistry(t *testing.T) {
	r, store := newRegistry(t)
	r.persistMappings(context.Background())
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.upserts != 0 {
		t.Fatalf("clean registry wrote %d mappings", store.upserts)
	}
}
