// Package tracker turns changes in chat.db into events.
//
// New messages are found by ROWID high-water mark, so none are missed even
// if a file notification is. Changes to existing messages (sent, delivered,
// read, edited, unsent, failed, played) are found by diffing a cheap state
// snapshot of recent messages against the previous one. Chat read markers
// are diffed the same way.
package tracker

import (
	"context"
	"log/slog"
	"time"

	"github.com/matthewmgamble/imsgd/internal/events"
	"github.com/matthewmgamble/imsgd/internal/imessage/appledate"
	"github.com/matthewmgamble/imsgd/internal/imessage/db"
	"github.com/matthewmgamble/imsgd/internal/present"
)

// Change names reported in message.updated events.
const (
	ChangeSent      = "sent"
	ChangeDelivered = "delivered"
	ChangeRead      = "read"
	ChangePlayed    = "played"
	ChangeEdited    = "edited"
	ChangeUnsent    = "unsent"
	ChangeFailed    = "failed"
)

type Options struct {
	DB        *db.DB
	Bus       *events.Bus
	Presenter *present.Presenter
	// Window is how far back changes to existing messages are tracked.
	// Default 7 days.
	Window time.Duration
	// BackfillAge suppresses events for new rows dated more than this far in
	// the past (e.g. history downloaded from iCloud). Default 1 hour.
	BackfillAge time.Duration
	// OnNewMessage, if set, is called for every new row (including
	// suppressed backfill) before events are published.
	OnNewMessage func(*db.Message)
	// OnMessageUpdated, if set, is called with each changed message.
	OnMessageUpdated func(m *db.Message, changes []string)
	Now              func() time.Time
}

type Tracker struct {
	o        Options
	maxRowID int64
	states   map[int64]db.MessageState
	chatRead map[string]int64
}

func New(o Options) *Tracker {
	if o.Window <= 0 {
		o.Window = 7 * 24 * time.Hour
	}
	if o.BackfillAge <= 0 {
		o.BackfillAge = time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Tracker{o: o, states: map[int64]db.MessageState{}}
}

// MessageEvent is the data of message.received/sent events.
type MessageEvent struct {
	Message present.Message `json:"message"`
}

// UpdateEvent is the data of message.updated events.
type UpdateEvent struct {
	Message present.Message `json:"message"`
	Changes []string        `json:"changes"`
}

// ChatReadEvent is the data of chat.read events.
type ChatReadEvent struct {
	ChatID string    `json:"chat_id"`
	ReadAt time.Time `json:"read_at"`
}

// Init records the current state without emitting events.
func (t *Tracker) Init(ctx context.Context) error {
	var err error
	if t.maxRowID, err = t.o.DB.MaxMessageRowID(ctx); err != nil {
		return err
	}
	states, err := t.o.DB.RecentMessageStates(ctx, t.o.Now().Add(-t.o.Window))
	if err != nil {
		return err
	}
	for _, s := range states {
		t.states[s.RowID] = s
	}
	if t.chatRead, err = t.o.DB.ChatReadStates(ctx); err != nil {
		return err
	}
	slog.Info("tracker initialised", "max_rowid", t.maxRowID, "tracked_messages", len(t.states))
	return nil
}

// MaxRowID is the highest message ROWID processed.
func (t *Tracker) MaxRowID() int64 { return t.maxRowID }

// Run processes ticks until ctx is done. Errors are logged and the work is
// retried on the next tick.
func (t *Tracker) Run(ctx context.Context, ticks <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if err := t.Tick(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("tracker tick failed", "err", err)
			}
		}
	}
}

// Tick checks for new messages, message updates and chat read changes.
func (t *Tracker) Tick(ctx context.Context) error {
	if err := t.newMessages(ctx); err != nil {
		return err
	}
	if err := t.updatedMessages(ctx); err != nil {
		return err
	}
	return t.chatReads(ctx)
}

func (t *Tracker) newMessages(ctx context.Context) error {
	cutoff := t.o.Now().Add(-t.o.BackfillAge)
	suppressed := 0
	for {
		msgs, err := t.o.DB.MessagesSince(ctx, t.maxRowID, "", 500)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			break
		}
		var emit []*db.Message
		for _, m := range msgs {
			t.states[m.RowID] = db.StateOf(m)
			if t.o.OnNewMessage != nil {
				t.o.OnNewMessage(m)
			}
			if m.Date != nil && m.Date.Before(cutoff) {
				suppressed++
				continue
			}
			emit = append(emit, m)
		}
		dtos, err := t.o.Presenter.Messages(ctx, emit)
		if err != nil {
			return err
		}
		for i, m := range emit {
			typ := events.MessageReceived
			if m.IsFromMe {
				typ = events.MessageSent
			}
			if _, err := t.o.Bus.Publish(typ, m.ChatGUID, MessageEvent{Message: dtos[i]}); err != nil {
				return err
			}
		}
		// Advance only after publishing so a failure retries the batch.
		t.maxRowID = msgs[len(msgs)-1].RowID
	}
	if suppressed > 0 {
		slog.Info("suppressed events for backfilled history", "messages", suppressed)
	}
	return nil
}

func (t *Tracker) updatedMessages(ctx context.Context) error {
	windowStart := t.o.Now().Add(-t.o.Window)
	states, err := t.o.DB.RecentMessageStates(ctx, windowStart)
	if err != nil {
		return err
	}
	changed := map[int64][]string{}
	var ids []int64
	seen := make(map[int64]bool, len(states))
	for _, cur := range states {
		seen[cur.RowID] = true
		if cur.RowID > t.maxRowID {
			continue // newer than our high-water mark; picked up as new next tick
		}
		prev, ok := t.states[cur.RowID]
		t.states[cur.RowID] = cur
		if !ok {
			continue // entered the window (e.g. date adjusted); nothing to compare
		}
		if c := Diff(prev, cur); len(c) > 0 {
			changed[cur.RowID] = c
			ids = append(ids, cur.RowID)
		}
	}
	// Forget messages that left the window or were deleted.
	for id := range t.states {
		if !seen[id] {
			delete(t.states, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	msgs, err := t.o.DB.MessagesByRowID(ctx, ids)
	if err != nil {
		return err
	}
	dtos, err := t.o.Presenter.Messages(ctx, msgs)
	if err != nil {
		return err
	}
	for i, m := range msgs {
		if t.o.OnMessageUpdated != nil {
			t.o.OnMessageUpdated(m, changed[m.RowID])
		}
		if _, err := t.o.Bus.Publish(events.MessageUpdated, m.ChatGUID, UpdateEvent{Message: dtos[i], Changes: changed[m.RowID]}); err != nil {
			return err
		}
	}
	return nil
}

// Diff lists what changed between two states of the same message.
func Diff(prev, cur db.MessageState) []string {
	var c []string
	if !prev.IsSent && cur.IsSent {
		c = append(c, ChangeSent)
	}
	if (!prev.IsDelivered && cur.IsDelivered) || (prev.DateDelivered == 0 && cur.DateDelivered != 0) {
		c = append(c, ChangeDelivered)
	}
	if (cur.DateRead != 0 && cur.DateRead != prev.DateRead) || (!prev.IsRead && cur.IsRead) {
		c = append(c, ChangeRead)
	}
	if cur.DatePlayed != 0 && cur.DatePlayed != prev.DatePlayed {
		c = append(c, ChangePlayed)
	}
	unsent := (cur.DateRetracted != 0 && cur.DateRetracted != prev.DateRetracted) ||
		(cur.PartCount < prev.PartCount && cur.DateEdited != prev.DateEdited)
	switch {
	case unsent:
		c = append(c, ChangeUnsent)
	case cur.DateEdited != 0 && cur.DateEdited != prev.DateEdited:
		c = append(c, ChangeEdited)
	}
	if prev.Error == 0 && cur.Error != 0 {
		c = append(c, ChangeFailed)
	}
	return c
}

func (t *Tracker) chatReads(ctx context.Context) error {
	cur, err := t.o.DB.ChatReadStates(ctx)
	if err != nil {
		return err
	}
	for guid, ts := range cur {
		prev, ok := t.chatRead[guid]
		if ok && ts > prev {
			readAt, _ := appledate.ToTime(ts)
			if _, err := t.o.Bus.Publish(events.ChatRead, guid, ChatReadEvent{ChatID: guid, ReadAt: readAt}); err != nil {
				return err
			}
		}
	}
	t.chatRead = cur
	return nil
}
