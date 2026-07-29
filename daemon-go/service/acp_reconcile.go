package service

import (
	"context"
	"fmt"
	"time"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

const acpAskOperationKind = "acp_ask"

func (d *PeerDelivery) recordACPAsk(ctx context.Context, cid, requestedFrom string, from, to *proto.Peer) (string, error) {
	if d.ops == nil || cid == "" || to == nil {
		return "", nil
	}
	fromID, fromName := requestedFrom, requestedFrom
	if from != nil {
		fromID, fromName = string(from.PeerID), string(from.DisplayName)
	}
	op, err := d.ops.CreateOperation(ctx, acpAskOperationKind, map[string]any{
		"correlation_id": cid, "from_peer_id": fromID, "from_peer_name": fromName,
		"to_peer_id": string(to.PeerID), "to_peer_name": string(to.DisplayName),
	}, nil)
	if err != nil {
		return "", fmt.Errorf("persist ACP ask: %w", err)
	}
	if op == nil {
		return "", fmt.Errorf("persist ACP ask: operation was not created")
	}
	strategy := "acp_prompt"
	if _, err := d.ops.StartAttempt(ctx, op.OperationID, &strategy, nil); err != nil {
		_, _ = d.ops.FailOperation(ctx, op.OperationID, "", nil, map[string]any{"reason": "acp_operation_start_failed"})
		return "", fmt.Errorf("start ACP ask operation: %w", err)
	}
	return op.OperationID, nil
}

func (d *PeerDelivery) settleACPAsk(ctx context.Context, operationID string, errText *string) {
	if d.ops == nil || operationID == "" {
		return
	}
	if errText != nil {
		_, _ = d.ops.FailOperation(ctx, operationID, "", nil, map[string]any{"reason": "acp_error", "detail": *errText})
		return
	}
	_, _ = d.ops.CompleteOperation(ctx, operationID, nil, map[string]any{"outcome": "acked"})
}

// ReconcileACPInflight closes prompt tasks lost by a daemon restart and queues one
// explicit closure for their original asker. It is idempotent because failed ops
// are no longer selected on the next startup.
func ReconcileACPInflight(ctx context.Context, store *state.Store, ttlSeconds float64, maxPerPeer int) int {
	if store == nil {
		return 0
	}
	n := 0
	for _, status := range []string{"queued", "running"} {
		ops, err := store.ListOperations(ctx, acpAskOperationKind, status)
		if err != nil {
			continue
		}
		for _, op := range ops {
			fromID, _ := op.Target["from_peer_id"].(string)
			if fromID == "" {
				continue
			}
			cid, _ := op.Target["correlation_id"].(string)
			fromName, _ := op.Target["from_peer_name"].(string)
			toID, _ := op.Target["to_peer_id"].(string)
			toName, _ := op.Target["to_peer_name"].(string)
			queued, err := store.EnqueueDelivery(ctx, state.QueuedDelivery{
				PeerID: fromID, Kind: state.DeliveryNotify, FromPeerID: stringPtr(toID),
				FromPeerName: toName, ToPeerName: fromName,
				Text: fmt.Sprintf("[ack #%s from @%s] ACP ask lost across daemon restart; please retry.", cid, toName),
			}, ttlSeconds, maxPerPeer, time.Time{})
			if err != nil || queued == nil {
				continue
			}
			if _, err := store.FailOperation(ctx, op.OperationID, "", nil, map[string]any{"reason": "daemon_restart_lost_acp_task"}); err != nil {
				continue
			}
			n++
		}
	}
	return n
}

func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
