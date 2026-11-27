package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/service"
)

// fakePeerPlug owns peers on its own path, the way a hosted agent plug does.
type fakePeerPlug struct {
	path    string
	err     error
	stopped []proto.PeerID
}

func (f *fakePeerPlug) Kind() string            { return "hosted" }
func (f *fakePeerPlug) Owns(p *proto.Peer) bool { return p.Path == f.path }
func (f *fakePeerPlug) StopPeer(_ context.Context, p *proto.Peer) error {
	f.stopped = append(f.stopped, p.PeerID)
	return f.err
}

func hostedPeer() *proto.Peer {
	return &proto.Peer{PeerID: "repow-hosted-1", DisplayName: "helper-hosted", Backend: proto.AgentType("hosted"), Circle: "default", Role: proto.RoleAgent, Path: "/hosted/helper"}
}

// kill_peer on a peer a plug owns outright stops it through the plug, then
// applies the operator close (closed, then unregistered) as for any backend;
// no pane is touched.
func TestKillPeerStopsAPlugOwnedPeerThenClosesIt(t *testing.T) {
	p := hostedPeer()
	reg := &fakeSpawnRegistry{peers: []*proto.Peer{p}}
	tmux := &fakeTmux{killOK: true}
	srv, svc := newSpawnTestHub(t, reg, tmux)
	plug := &fakePeerPlug{path: p.Path}
	svc.WithPeerContainers(plug)

	resp := postSpawnJSON(t, srv, "/kill-peer", KillPeerRequest{PeerIdentifier: string(p.PeerID)})
	defer resp.Body.Close()
	var out KillResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || !out.OK || out.Container != "hosted" || out.TmuxKilled != nil {
		t.Fatalf("status %d, %+v; want 200 ok via the hosted container", resp.StatusCode, out)
	}
	if len(plug.stopped) != 1 || plug.stopped[0] != p.PeerID {
		t.Fatalf("stopped %v, want %s", plug.stopped, p.PeerID)
	}
	if len(reg.closed) != 1 || reg.closed[0] != p.PeerID || len(reg.unregistered) != 1 {
		t.Fatalf("closed %v unregistered %v; want the operator close", reg.closed, reg.unregistered)
	}
	if len(tmux.killed) != 0 {
		t.Fatalf("killed panes %v, want none", tmux.killed)
	}
}

// A plug that cannot stop its peer fails the kill loudly and keeps the peer.
func TestKillPeerKeepsAPlugOwnedPeerItCouldNotStop(t *testing.T) {
	p := hostedPeer()
	reg := &fakeSpawnRegistry{peers: []*proto.Peer{p}}
	srv, svc := newSpawnTestHub(t, reg, &fakeTmux{killOK: true})
	svc.WithPeerContainers(&fakePeerPlug{path: p.Path, err: errors.New("runtime unreachable")})

	resp := postSpawnJSON(t, srv, "/kill-peer", KillPeerRequest{PeerIdentifier: string(p.PeerID)})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if detail, _ := body["detail"].(map[string]any); detail["error"] != "kill_failed" || detail["container"] != "hosted" {
		t.Fatalf("body %v, want detail.error kill_failed from the hosted container", body)
	}
	if len(reg.closed) != 0 || len(reg.unregistered) != 0 {
		t.Fatalf("closed %v unregistered %v; want the peer kept", reg.closed, reg.unregistered)
	}
}

// A peer no plug owns takes the pane path exactly as before.
func TestPeerContainerForLeavesOtherPeersToThePanePath(t *testing.T) {
	svc := service.NewSpawnService(&fakeTmux{}, nil, nil, nil).WithPeerContainers(&fakePeerPlug{path: "/hosted/helper"})
	if svc.PeerContainerFor(&proto.Peer{Path: "/work/x"}) != nil {
		t.Fatal("a pane-backed peer was claimed by the hosted plug")
	}
	if svc.PeerContainerFor(hostedPeer()) == nil {
		t.Fatal("the hosted peer was not claimed")
	}
}

// DELETE /peers/{name} is an operator close too: a peer a plug owns outright
// is stopped through the plug first, so closing its identity never leaves its
// execution running. A stop that fails keeps the peer.
func TestDeletePeerStopsAPlugOwnedPeerFirst(t *testing.T) {
	h := newTestHub(t)
	tmux := &fakeTmux{killOK: true}
	svc := newHermeticSpawnService(t, tmux, service.NewFileOwnership("test-host", tmux.Probe), nil, []string{t.TempDir()})
	h.WithSpawn(svc, h.reg, service.NewAskTracker(0), "test-host", proto.CircleBoundarySession)
	plug := &fakePeerPlug{path: "/container/helper", err: errors.New("runtime unreachable")}
	svc.WithPeerContainers(plug)
	mux := http.NewServeMux()
	h.Routes(mux)

	path, circle := plug.path, "default"
	registered := postLifecycleJSON(t, mux, "/peers", RegisterPeerRequest{Name: "helper", Path: &path, Backend: proto.AgentCodex, Circle: &circle})
	var peer RegisterResponse
	if err := json.Unmarshal(registered.Body.Bytes(), &peer); err != nil || registered.Code != http.StatusOK {
		t.Fatalf("register: %d %s", registered.Code, registered.Body.String())
	}

	if failed := deleteLifecycle(t, mux, "/peers/"+peer.PeerID); failed.Code != http.StatusInternalServerError {
		t.Fatalf("delete with a failing stop = %d %s, want 500", failed.Code, failed.Body.String())
	}
	if _, ok := h.reg.GetPeer(proto.PeerID(peer.PeerID)); !ok {
		t.Fatal("a peer whose stop failed must stay registered")
	}

	plug.err = nil
	if closed := deleteLifecycle(t, mux, "/peers/"+peer.PeerID); closed.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", closed.Code, closed.Body.String())
	}
	if _, ok := h.reg.GetPeer(proto.PeerID(peer.PeerID)); ok || len(plug.stopped) != 2 {
		t.Fatalf("delete must stop through the plug, then close: stopped=%v", plug.stopped)
	}
}
