package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/repowire/repowire/daemon-go/config"
	"github.com/repowire/repowire/daemon-go/service"
	"github.com/repowire/repowire/daemon-go/state"
)

func hasPrefixed(texts []string, prefix string) bool {
	return slices.ContainsFunc(texts, func(s string) bool { return strings.HasPrefix(s, prefix) })
}

func TestOperatorReassignMovesTheAskAndTellsEveryone(t *testing.T) {
	ctx := context.Background()
	alpha, beta, gamma := alphaPeer(), betaPeer(), gammaPeer()
	r := newDelegateRig(t, alpha, beta, gamma)
	cid := r.ask(t, alpha, beta, "who owns the release?")
	// A copy still queued for beta must not reach it after the move.
	if _, err := r.store.EnqueueDelivery(ctx, state.QueuedDelivery{PeerID: string(beta.PeerID), Kind: state.DeliveryAsk, CorrelationID: &cid,
		FromPeerName: "alpha", ToPeerName: "beta", Text: "who owns the release?"}, 3600, 10, time.Time{}); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct{ to, reason string }{
		"no reason": {"gamma", ""}, "to the asker": {"alpha", "x"}, "to the holder": {"beta", "x"}, "unknown peer": {"nobody", "x"},
		"too long": {"gamma", strings.Repeat("x", maxActionReason+1)},
	} {
		if _, err := r.h.reassignAsk(ctx, cid, tc.to, tc.reason); err == nil {
			t.Fatalf("%s: reassign succeeded", name)
		}
	}

	res, err := r.h.reassignAsk(ctx, cid, "gamma", "release owner")
	if err != nil || res.Holder != "gamma" || res.DroppedQueued != 1 {
		t.Fatalf("reassign: %+v %v", res, err)
	}
	if !hasPrefixed(frameTexts(r.wire.received(gamma.PeerID)), "[reassigned by the operator from @beta for @alpha: release owner]\nwho owns the release?") {
		t.Fatalf("gamma received %q", frameTexts(r.wire.received(gamma.PeerID)))
	}
	if !hasPrefixed(frameTexts(r.wire.received(beta.PeerID)), "[ask #"+cid+" moved to @gamma by the operator: release owner]") {
		t.Fatalf("beta received %q", frameTexts(r.wire.received(beta.PeerID)))
	}
	if !hasPrefixed(frameTexts(r.wire.received(alpha.PeerID)), "[your ask #"+cid+" moved from @beta to @gamma by the operator: release owner]") {
		t.Fatalf("alpha received %q", frameTexts(r.wire.received(alpha.PeerID)))
	}
	ask, _ := r.asks.Get(cid)
	if ask.ToPeerID != gamma.PeerID || len(ask.Delegations) != 1 || ask.Delegations[0].By != operatorActor || ask.Delegations[0].Reason != "release owner" {
		t.Fatalf("ask after reassign: %+v", ask)
	}
	if left, _ := r.store.ListDeliveries(ctx, string(beta.PeerID), 10, time.Time{}); len(left) != 0 {
		t.Fatalf("beta still has %d queued copies", len(left))
	}

	// The former holder can neither answer nor get the ask back.
	if status, _ := r.ack(t, cid, beta, "stale"); status != http.StatusConflict {
		t.Fatalf("beta's ack: %d", status)
	}
	if _, err := r.h.reassignAsk(ctx, cid, "beta", "back"); err == nil {
		t.Fatal("moved back to a former holder")
	}
	if status, _ := r.ack(t, cid, gamma, "me"); status != http.StatusOK {
		t.Fatalf("gamma's ack: %d", status)
	}
}

func TestOperatorCancelClosesTheAskAndTellsBothEnds(t *testing.T) {
	ctx := context.Background()
	alpha, beta := alphaPeer(), betaPeer()
	r := newDelegateRig(t, alpha, beta)
	cid := r.ask(t, alpha, beta, "review?")
	if _, err := r.h.cancelAsk(ctx, cid, ""); err == nil {
		t.Fatal("cancel without a reason")
	}
	res, err := r.h.cancelAsk(ctx, cid, "no longer needed")
	if err != nil || res.Action != "cancel" {
		t.Fatalf("cancel: %+v %v", res, err)
	}
	if !hasPrefixed(frameTexts(r.wire.received(alpha.PeerID)), "[ask #"+cid+" cancelled by the operator] no longer needed") {
		t.Fatalf("alpha received %q", frameTexts(r.wire.received(alpha.PeerID)))
	}
	if !hasPrefixed(frameTexts(r.wire.received(beta.PeerID)), "[ask #"+cid+" from @alpha cancelled by the operator: no longer needed]") {
		t.Fatalf("beta received %q", frameTexts(r.wire.received(beta.PeerID)))
	}
	if ask, _ := r.asks.Get(cid); !ask.Closed || ask.CloseReason != "cancelled" {
		t.Fatalf("ask after cancel: %+v", ask)
	}
	if status, _ := r.ack(t, cid, beta, "late"); status != http.StatusGone {
		t.Fatalf("holder's late ack: %d", status)
	}

	// A result already waiting for the asker is not cancelled.
	stashed := r.ask(t, alpha, beta, "another")
	if !r.asks.SetPendingReply(ctx, stashed, "[ack #"+stashed+" from @beta] done", nil, false) {
		t.Fatal("stash refused")
	}
	if _, err := r.h.cancelAsk(ctx, stashed, "too late"); err == nil {
		t.Fatal("cancelled an ask with a stashed result")
	}
}

func TestOperatorNudgeIsRateLimited(t *testing.T) {
	ctx := context.Background()
	alpha, beta := alphaPeer(), betaPeer()
	r := newDelegateRig(t, alpha, beta)
	r.h.deliveryTraces = r.store // the ledger, as in the daemon: the limit reads it
	cid := r.ask(t, alpha, beta, "status?")
	if _, err := r.h.nudgeAsk(ctx, cid, "any news?"); err != nil {
		t.Fatal(err)
	}
	if !hasPrefixed(frameTexts(r.wire.received(beta.PeerID)), "[nudge from the operator: ask #"+cid+" from @alpha, open") {
		t.Fatalf("beta received %q", frameTexts(r.wire.received(beta.PeerID)))
	}
	if _, err := r.h.nudgeAsk(ctx, cid, ""); err == nil || !strings.Contains(err.Error(), "nudged_recently") {
		t.Fatalf("second nudge: %v", err)
	}
	// A holder whose reply already waits for the asker is not nudged.
	stashed := r.ask(t, alpha, beta, "another")
	if !r.asks.SetPendingReply(ctx, stashed, "[ack #"+stashed+" from @beta] done", nil, false) {
		t.Fatal("stash refused")
	}
	if _, err := r.h.nudgeAsk(ctx, stashed, ""); err == nil {
		t.Fatal("nudged a holder whose reply is stashed")
	}
}

// The action routes take only the daemon token: no tokenless pass and no
// local-dashboard header pass.
func TestWireActionRoutesNeedTheOperatorToken(t *testing.T) {
	alpha, beta, gamma := alphaPeer(), betaPeer(), gammaPeer()
	r := newDelegateRig(t, alpha, beta, gamma)
	cid := r.ask(t, alpha, beta, "review?")
	r.h.WithWire(service.NewWire(r.asks, r.store, r.reg, config.Defaults().Wire.Stuck))
	r.h.store = r.store
	route := func(token string) (*httptest.Server, func(headers map[string]string) int) {
		r.h.authToken = token
		mux := http.NewServeMux()
		r.h.registerWireRoutes(mux)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv, func(headers map[string]string) int {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/wire/threads/"+cid+"/nudge", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp.StatusCode
		}
	}
	_, post := route("")
	if status := post(nil); status != http.StatusForbidden {
		t.Fatalf("tokenless daemon: %d", status)
	}
	srv, post := route("tok")
	for name, headers := range map[string]map[string]string{
		"no token":    nil,
		"wrong token": {"Authorization": "Bearer nope"},
		"same-origin": {"Sec-Fetch-Site": "same-origin", "Referer": srv.URL + "/"},
	} {
		if status := post(headers); status != http.StatusUnauthorized {
			t.Fatalf("%s: %d", name, status)
		}
	}
	if status := post(map[string]string{"Authorization": "Bearer tok"}); status != http.StatusOK {
		t.Fatalf("operator token: %d", status)
	}
}
