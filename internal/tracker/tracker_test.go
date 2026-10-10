package tracker

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/matthewmgamble/imsgd/internal/events"
	"github.com/matthewmgamble/imsgd/internal/imessage/appledate"
	"github.com/matthewmgamble/imsgd/internal/imessage/db"
	"github.com/matthewmgamble/imsgd/internal/imessage/fixture"
	"github.com/matthewmgamble/imsgd/internal/present"
)

var ctx = context.Background()

func at(min int) time.Time { return time.Date(2026, 3, 1, 10, min, 0, 0, time.UTC) }

type harness struct {
	t     *testing.T
	f     *fixture.Builder
	tr    *Tracker
	sub   *events.Subscription
	chat  int64
	alice int64
	now   time.Time
}

func newHarness(t *testing.T) *harness {
	f := fixture.New(t)
	h := &harness{t: t, f: f, now: at(30)}
	h.alice = f.Handle("+15550000001", "iMessage")
	h.chat = f.Chat("any;-;+15550000001", fixture.StyleDirect, "", h.alice)
	f.Message(fixture.Msg{Chat: h.chat, Handle: h.alice, Text: "existing", Date: at(1)})

	d, err := db.Open(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	bus := events.NewBus(100)
	h.sub, _, _ = bus.Subscribe(events.Filter{}, "", 100)
	h.tr = New(Options{DB: d, Bus: bus, Presenter: &present.Presenter{DB: d}, Now: func() time.Time { return h.now }})
	if err := h.tr.Init(ctx); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) tick() []events.Event {
	h.t.Helper()
	if err := h.tr.Tick(ctx); err != nil {
		h.t.Fatal(err)
	}
	var out []events.Event
	for {
		select {
		case ev := <-h.sub.C():
			out = append(out, ev)
		default:
			return out
		}
	}
}

func decode[T any](t *testing.T, ev events.Event) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(ev.Data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestInitEmitsNothing(t *testing.T) {
	h := newHarness(t)
	if evs := h.tick(); len(evs) != 0 {
		t.Fatalf("unexpected events: %v", evs)
	}
}

func TestNewMessages(t *testing.T) {
	h := newHarness(t)
	_, in := h.f.Message(fixture.Msg{Chat: h.chat, Handle: h.alice, Text: "incoming", Date: at(20)})
	_, out := h.f.Message(fixture.Msg{Chat: h.chat, Handle: h.alice, FromMe: true, Text: "outgoing", Date: at(21)})
	evs := h.tick()
	if len(evs) != 2 {
		t.Fatalf("got %d events", len(evs))
	}
	if evs[0].Type != events.MessageReceived || evs[1].Type != events.MessageSent {
		t.Fatalf("types: %s, %s", evs[0].Type, evs[1].Type)
	}
	m0 := decode[MessageEvent](t, evs[0]).Message
	if m0.ID != in || m0.Text != "incoming" || evs[0].ChatID != "any;-;+15550000001" {
		t.Fatalf("event 0 = %+v", m0)
	}
	if decode[MessageEvent](t, evs[1]).Message.ID != out {
		t.Fatal("event 1 wrong message")
	}
	if evs := h.tick(); len(evs) != 0 {
		t.Fatalf("re-emitted: %v", evs)
	}
}

func TestBackfillSuppressed(t *testing.T) {
	h := newHarness(t)
	var seen []string
	h.tr.o.OnNewMessage = func(m *db.Message) { seen = append(seen, m.GUID) }
	_, old := h.f.Message(fixture.Msg{Chat: h.chat, Handle: h.alice, Text: "from icloud", Date: at(30).Add(-48 * time.Hour)})
	_, fresh := h.f.Message(fixture.Msg{Chat: h.chat, Handle: h.alice, Text: "fresh", Date: at(29)})
	evs := h.tick()
	if len(evs) != 1 || decode[MessageEvent](t, evs[0]).Message.ID != fresh {
		t.Fatalf("events = %v", evs)
	}
	if !slices.Equal(seen, []string{old, fresh}) {
		t.Fatalf("OnNewMessage saw %v", seen)
	}
	if evs := h.tick(); len(evs) != 0 {
		t.Fatal("backfilled message emitted later")
	}
}

func TestUpdates(t *testing.T) {
	h := newHarness(t)
	id, guid := h.f.Message(fixture.Msg{Chat: h.chat, Handle: h.alice, FromMe: true, Text: "hi", Date: at(20)})
	h.tick() // message.sent

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := h.f.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(want ...string) {
		t.Helper()
		evs := h.tick()
		if len(want) == 0 {
			if len(evs) != 0 {
				t.Fatalf("unexpected events %v", evs)
			}
			return
		}
		if len(evs) != 1 || evs[0].Type != events.MessageUpdated {
			t.Fatalf("events = %v", evs)
		}
		u := decode[UpdateEvent](t, evs[0])
		if u.Message.ID != guid || !slices.Equal(u.Changes, want) {
			t.Fatalf("changes = %v, want %v", u.Changes, want)
		}
	}

	exec(`UPDATE message SET is_sent = 1 WHERE ROWID = ?`, id)
	expect(ChangeSent)
	exec(`UPDATE message SET is_delivered = 1, date_delivered = ? WHERE ROWID = ?`, appledate.FromTime(at(21)), id)
	expect(ChangeDelivered)
	exec(`UPDATE message SET date_read = ? WHERE ROWID = ?`, appledate.FromTime(at(22)), id)
	expect(ChangeRead)
	expect() // no change, no event
	exec(`UPDATE message SET date_edited = ?, part_count = 1 WHERE ROWID = ?`, appledate.FromTime(at(23)), id)
	expect(ChangeEdited)
	exec(`UPDATE message SET date_edited = ?, part_count = 0 WHERE ROWID = ?`, appledate.FromTime(at(24)), id)
	expect(ChangeUnsent)

	id2, _ := h.f.Message(fixture.Msg{Chat: h.chat, Handle: h.alice, FromMe: true, Text: "fails", Date: at(25)})
	h.tick()
	exec(`UPDATE message SET error = 22 WHERE ROWID = ?`, id2)
	evs := h.tick()
	if len(evs) != 1 || !slices.Equal(decode[UpdateEvent](t, evs[0]).Changes, []string{ChangeFailed}) {
		t.Fatalf("failed events = %v", evs)
	}
	if m := decode[UpdateEvent](t, evs[0]).Message; m.Error != 22 {
		t.Fatalf("message error = %d", m.Error)
	}
}

func TestChangesOutsideWindowIgnored(t *testing.T) {
	h := newHarness(t)
	h.now = at(30).Add(30 * 24 * time.Hour) // "existing" is now a month old
	h.tick()
	h.f.DB.Exec(`UPDATE message SET is_read = 1`)
	if evs := h.tick(); len(evs) != 0 {
		t.Fatalf("events for old message: %v", evs)
	}
}

func TestChatRead(t *testing.T) {
	h := newHarness(t)
	h.f.SetLastRead(h.chat, at(25))
	evs := h.tick()
	if len(evs) != 1 || evs[0].Type != events.ChatRead {
		t.Fatalf("events = %v", evs)
	}
	cr := decode[ChatReadEvent](t, evs[0])
	if cr.ChatID != "any;-;+15550000001" || !cr.ReadAt.Equal(at(25)) {
		t.Fatalf("chat.read = %+v", cr)
	}
	if evs := h.tick(); len(evs) != 0 {
		t.Fatal("re-emitted chat.read")
	}
	// New chats are recorded silently.
	h.f.Chat("any;-;new@example.com", fixture.StyleDirect, "", h.alice)
	if evs := h.tick(); len(evs) != 0 {
		t.Fatalf("new chat emitted %v", evs)
	}
}

func TestDiff(t *testing.T) {
	base := db.MessageState{RowID: 1, PartCount: 1, DateEdited: 0}
	cases := []struct {
		cur  db.MessageState
		want []string
	}{
		{db.MessageState{RowID: 1, PartCount: 1, IsRead: true}, []string{ChangeRead}},
		{db.MessageState{RowID: 1, PartCount: 1, DatePlayed: 5}, []string{ChangePlayed}},
		{db.MessageState{RowID: 1, PartCount: 0, DateRetracted: 9}, []string{ChangeUnsent}},
		{db.MessageState{RowID: 1, PartCount: 1, IsSent: true, IsDelivered: true}, []string{ChangeSent, ChangeDelivered}},
		{base, nil},
	}
	for _, c := range cases {
		if got := Diff(base, c.cur); !slices.Equal(got, c.want) {
			t.Errorf("Diff(%+v) = %v, want %v", c.cur, got, c.want)
		}
	}
}
