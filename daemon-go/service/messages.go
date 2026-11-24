package service

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/repowire/repowire/daemon-go/proto"
	"github.com/repowire/repowire/daemon-go/state"
)

// ============================================================================
// Messages: the durable record behind every message an agent peer receives
// (rfcs/message-ids-pull-delegate.md). PeerDelivery records a notify, an ask,
// or an ask reply here before choosing a transport, so the frame can carry a
// message_id the recipient can later list, peek at, or fetch.
//
// A recipient that chose pull delivery is sent a one-line nudge naming the
// id instead of the body. The choice is durable per peer_id. Records expire
// under the retention policy (service/retention.go).
// ============================================================================

// Message delivery modes.
const (
	MessageDeliveryPush = "push"
	MessageDeliveryPull = "pull"
)

const (
	nudgePreviewRunes = 80
)

// MessageStore is the persistence seam; *state.Store satisfies it.
type MessageStore interface {
	InsertPeerMessage(ctx context.Context, msg *state.PeerMessage) error
	PeerMessage(ctx context.Context, id string) (*state.PeerMessage, error)
	ListPeerMessages(ctx context.Context, filter state.PeerMessageFilter) ([]state.PeerMessage, error)
	MarkPeerMessageRead(ctx context.Context, id string) (bool, error)
	DeletePeerMessage(ctx context.Context, id string) error
	SetPeerMessageDelivery(ctx context.Context, peerID, mode string) error
	PeerMessageDeliveries(ctx context.Context) (map[string]string, error)
}

// Messages records and serves agent peer messages.
type Messages struct {
	store MessageStore

	mu    sync.Mutex
	modes map[proto.PeerID]string // loaded on first use; guarded by mu
}

// NewMessages wires the message record over store.
func NewMessages(store MessageStore) *Messages {
	return &Messages{store: store}
}

// NewNotifyDeliveryID mints a notify's delivery id, which is also its trace id.
func NewNotifyDeliveryID() string { return "notif-delivery-" + uuid.NewString()[:8] }

// Mode is id's delivery mode: pull when the peer chose it, else push.
func (m *Messages) Mode(ctx context.Context, id proto.PeerID) string {
	if m == nil {
		return MessageDeliveryPush
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadModesLocked(ctx); err != nil {
		log.Printf("messages: load delivery modes: %v", err)
		return MessageDeliveryPush
	}
	if m.modes[id] == MessageDeliveryPull {
		return MessageDeliveryPull
	}
	return MessageDeliveryPush
}

// SetMode records id's delivery mode, push or pull. The write and the cache
// change together under the lock, so concurrent calls cannot leave the cache
// disagreeing with the store.
func (m *Messages) SetMode(ctx context.Context, id proto.PeerID, mode string) error {
	if mode != MessageDeliveryPush && mode != MessageDeliveryPull {
		return fmt.Errorf("message delivery must be %s or %s", MessageDeliveryPush, MessageDeliveryPull)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadModesLocked(ctx); err != nil {
		return err
	}
	if err := m.store.SetPeerMessageDelivery(ctx, string(id), mode); err != nil {
		return err
	}
	m.modes[id] = mode
	return nil
}

// loadModesLocked fills the mode cache from the store once. Caller holds m.mu.
func (m *Messages) loadModesLocked(ctx context.Context) error {
	if m.modes != nil {
		return nil
	}
	loaded, err := m.store.PeerMessageDeliveries(ctx)
	if err != nil {
		return err
	}
	m.modes = make(map[proto.PeerID]string, len(loaded))
	for peerID, mode := range loaded {
		m.modes[proto.PeerID(peerID)] = mode
	}
	return nil
}

// Record stores msg for its recipient. A nil receiver records nothing (no
// state store), and so does a failed insert: the message is then pushed as
// before, without an id, rather than dropped.
func (m *Messages) Record(ctx context.Context, msg state.PeerMessage) *state.PeerMessage {
	if m == nil {
		return nil
	}
	if err := m.store.InsertPeerMessage(ctx, &msg); err != nil {
		log.Printf("messages: record %s for %s: %v", msg.Kind, msg.PeerID, err)
		return nil
	}
	return &msg
}

// Get returns one message ((nil, nil) when unknown).
func (m *Messages) Get(ctx context.Context, id string) (*state.PeerMessage, error) {
	return m.store.PeerMessage(ctx, id)
}

// List returns one recipient's messages, newest first.
func (m *Messages) List(ctx context.Context, filter state.PeerMessageFilter) ([]state.PeerMessage, error) {
	return m.store.ListPeerMessages(ctx, filter)
}

// MarkRead records that the body reached its recipient. Nil-safe and
// idempotent.
func (m *Messages) MarkRead(ctx context.Context, msg *state.PeerMessage) {
	if m == nil || msg == nil {
		return
	}
	if _, err := m.store.MarkPeerMessageRead(ctx, msg.ID); err != nil {
		log.Printf("messages: mark %s read: %v", msg.ID, err)
	}
}

// Discard drops the record of a message whose send failed outright, so the
// recipient never lists a message it was not sent. Nil-safe.
func (m *Messages) Discard(ctx context.Context, msg *state.PeerMessage) {
	if m == nil || msg == nil {
		return
	}
	if err := m.store.DeletePeerMessage(ctx, msg.ID); err != nil {
		log.Printf("messages: discard %s: %v", msg.ID, err)
	}
}

// wireText is what goes on the wire for msg: the body, or for a pull
// recipient a nudge naming the message. pull reports which.
func (m *Messages) wireText(ctx context.Context, msg *state.PeerMessage, body string) (text string, pull bool) {
	if msg == nil || m.Mode(ctx, proto.PeerID(msg.PeerID)) != MessageDeliveryPull {
		return body, false
	}
	return Nudge(msg), true
}

// Nudge is the one-line pointer a pull recipient receives instead of msg's
// body. An ask nudge carries no hint of its own: the ask frame adds the ack
// hint after it, as it does after a body.
func Nudge(msg *state.PeerMessage) string {
	from := strings.TrimPrefix(firstNonEmpty(msg.FromName, msg.FromPeerID, "unknown"), "@")
	head := fmt.Sprintf("[message %s: %s from @%s, %d chars", msg.ID, msg.Kind, from, utf8.RuneCountInString(msg.Text))
	if n := len(msg.Attachments); n > 0 {
		head += fmt.Sprintf(", %d attachment(s)", n)
	}
	head += "]"
	if preview := MessagePreview(msg.Text); preview != "" {
		head += " " + preview
	}
	return head + fmt.Sprintf("\n↳ read_message(%q)", msg.ID)
}

// MessagePreview is the body's first non-empty line, clipped.
func MessagePreview(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > nudgePreviewRunes {
			runes := []rune(line)
			return string(runes[:nudgePreviewRunes-1]) + "…"
		}
		return line
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
