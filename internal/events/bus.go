// Package events is the in-process event bus. Producers publish events;
// WebSocket connections and the webhook dispatcher subscribe.
//
// Recent events are kept in a ring buffer so a client that reconnects with
// its last event ID can resume without gaps. IDs embed a per-process boot
// ID, so IDs from before a restart are recognised as unresumable.
package events

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event types.
const (
	MessageReceived = "message.received"
	MessageSent     = "message.sent"
	MessageUpdated  = "message.updated"
	ChatRead        = "chat.read"
	SendUpdated     = "send.updated"
)

// Types lists every event type, for validation and documentation.
var Types = []string{MessageReceived, MessageSent, MessageUpdated, ChatRead, SendUpdated}

// Event is the envelope delivered over WebSocket and webhooks.
type Event struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`

	// ChatID scopes the event to a chat for subscription filters. Not serialized.
	ChatID string `json:"-"`
	// Owner, if set, restricts delivery to subscribers acting for that API
	// key (e.g. send.updated for a key's own sends). Not serialized.
	Owner string `json:"-"`
	seq   uint64
}

// Bus fans events out to subscribers and keeps a replay buffer.
type Bus struct {
	mu     sync.Mutex
	bootID string
	seq    uint64
	ring   []Event
	start  int // index of the oldest event in ring
	count  int
	subs   map[*Subscription]struct{}
	now    func() time.Time
}

// DefaultBufferSize is the number of events kept for replay.
const DefaultBufferSize = 10000

// NewBus creates a bus that keeps the last size events for replay.
func NewBus(size int) *Bus {
	if size <= 0 {
		size = DefaultBufferSize
	}
	b := make([]byte, 4)
	rand.Read(b)
	return &Bus{
		bootID: hex.EncodeToString(b),
		ring:   make([]Event, size),
		subs:   map[*Subscription]struct{}{},
		now:    time.Now,
	}
}

// Publish assigns an ID and timestamp to an event and delivers it. data is
// marshalled to JSON. Slow subscribers whose buffers are full are closed
// rather than blocking the publisher.
func (b *Bus) Publish(typ, chatID string, data any) (Event, error) {
	return b.PublishOwned(typ, chatID, "", data)
}

// PublishOwned publishes an event visible only to subscribers whose filter
// Owner equals owner.
func (b *Bus) PublishOwned(typ, chatID, owner string, data any) (Event, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, fmt.Errorf("marshal %s event: %w", typ, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	ev := Event{
		ID:        b.bootID + "-" + strconv.FormatUint(b.seq, 10),
		Type:      typ,
		CreatedAt: b.now().UTC(),
		Data:      raw,
		ChatID:    chatID,
		Owner:     owner,
		seq:       b.seq,
	}
	// Append to the ring.
	idx := (b.start + b.count) % len(b.ring)
	b.ring[idx] = ev
	if b.count < len(b.ring) {
		b.count++
	} else {
		b.start = (b.start + 1) % len(b.ring)
	}
	for s := range b.subs {
		if !s.filter.Match(ev) {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			// Too slow: drop the subscriber; it can resume via last_event_id.
			b.closeLocked(s, ErrSlowConsumer)
		}
	}
	return ev, nil
}

// LastID returns the most recent event ID ("" if none).
func (b *Bus) LastID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seq == 0 {
		return ""
	}
	return b.bootID + "-" + strconv.FormatUint(b.seq, 10)
}

// Filter selects events for a subscriber. Zero value matches everything.
type Filter struct {
	// Types are exact types or prefixes ending in ".*" (e.g. "message.*").
	Types   []string
	ChatIDs []string
	// Owner is the subscriber's API key name; owned events for other keys
	// are never delivered.
	Owner string
}

func (f Filter) Match(ev Event) bool {
	if ev.Owner != "" && ev.Owner != f.Owner {
		return false
	}
	if len(f.Types) > 0 {
		ok := false
		for _, t := range f.Types {
			if t == ev.Type || t == "*" || (strings.HasSuffix(t, ".*") && strings.HasPrefix(ev.Type, t[:len(t)-1])) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(f.ChatIDs) > 0 {
		ok := false
		for _, c := range f.ChatIDs {
			if c == ev.ChatID {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// ValidateTypes checks a type filter list.
func ValidateTypes(types []string) error {
	for _, t := range types {
		if t == "*" {
			continue
		}
		valid := false
		for _, known := range Types {
			if t == known || (strings.HasSuffix(t, ".*") && strings.HasPrefix(known, t[:len(t)-1])) {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("unknown event type %q", t)
		}
	}
	return nil
}

// Subscription receives events on C until closed.
type Subscription struct {
	bus    *Bus
	ch     chan Event
	filter Filter
	done   chan struct{}
	err    error
	once   sync.Once
}

// C delivers events.
func (s *Subscription) C() <-chan Event { return s.ch }

// Done is closed when the subscription ends (Close, or too slow).
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Err reports why the subscription ended.
func (s *Subscription) Err() error {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	return s.err
}

// Close ends the subscription.
func (s *Subscription) Close() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	s.bus.closeLocked(s, nil)
}

func (b *Bus) closeLocked(s *Subscription, err error) {
	if _, ok := b.subs[s]; !ok {
		return
	}
	delete(b.subs, s)
	s.err = err
	s.once.Do(func() { close(s.done) })
}

// ErrSlowConsumer ends a subscription whose buffer filled up.
var ErrSlowConsumer = fmt.Errorf("subscriber too slow; events dropped")

// Subscribe registers a subscriber. If lastID is non-empty, events after it
// that are still buffered are returned as backlog (filtered); resumed is
// false when lastID is unknown, from a previous process, or older than the
// buffer, in which case the client must resynchronise via the REST API.
func (b *Bus) Subscribe(f Filter, lastID string, buffer int) (sub *Subscription, backlog []Event, resumed bool) {
	if buffer <= 0 {
		buffer = 256
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	sub = &Subscription{bus: b, ch: make(chan Event, buffer), filter: f, done: make(chan struct{})}
	b.subs[sub] = struct{}{}

	if lastID == "" {
		return sub, nil, true
	}
	boot, seqStr, ok := strings.Cut(lastID, "-")
	seq, err := strconv.ParseUint(seqStr, 10, 64)
	if !ok || err != nil || boot != b.bootID || seq > b.seq {
		return sub, nil, false
	}
	oldest := b.seq - uint64(b.count) + 1 // seq of the oldest buffered event
	if b.count == 0 || seq+1 < oldest {
		return sub, nil, seq == b.seq
	}
	for i := 0; i < b.count; i++ {
		ev := b.ring[(b.start+i)%len(b.ring)]
		if ev.seq > seq && f.Match(ev) {
			backlog = append(backlog, ev)
		}
	}
	return sub, backlog, true
}
