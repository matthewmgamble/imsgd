package send_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/matthewmgamble/imsgd/internal/events"
	"github.com/matthewmgamble/imsgd/internal/imessage/db"
	"github.com/matthewmgamble/imsgd/internal/imessage/fixture"
	"github.com/matthewmgamble/imsgd/internal/present"
	"github.com/matthewmgamble/imsgd/internal/send"
	"github.com/matthewmgamble/imsgd/internal/send/sendtest"
	"github.com/matthewmgamble/imsgd/internal/store"
	"github.com/matthewmgamble/imsgd/internal/tracker"
)

type harness struct {
	t      *testing.T
	f      *fixture.Builder
	runner *sendtest.Runner
	sender *send.Sender
	store  *store.Store
	bus    *events.Bus
	chat   string
	cancel context.CancelFunc
}

func newHarness(t *testing.T, opts ...func(*send.Options)) *harness {
	t.Helper()
	f := fixture.New(t)
	h := f.Handle("+15550000001", "iMessage")
	f.Chat("any;-;+15550000001", fixture.StyleDirect, "", h)

	d, err := db.Open(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	st, err := store.Open(filepath.Join(t.TempDir(), "imsgd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus(1000)
	runner := &sendtest.Runner{F: f}
	o := send.Options{
		Store: st, Bus: bus, Runner: runner, StagingDir: t.TempDir(),
		Present:       func(s *store.Send) any { return present.SendOf(s) },
		SweepInterval: 20 * time.Millisecond,
	}
	for _, fn := range opts {
		fn(&o)
	}
	sender := send.New(o)
	trk := tracker.New(tracker.Options{
		DB: d, Bus: bus, Presenter: &present.Presenter{DB: d},
		OnNewMessage: sender.OnNewMessage, OnMessageUpdated: sender.OnMessageUpdated,
	})
	if err := trk.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sender.Run(ctx)
	// Tick the tracker continuously, standing in for the file watcher.
	go func() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				trk.Tick(ctx)
			}
		}
	}()
	return &harness{t: t, f: f, runner: runner, sender: sender, store: st, bus: bus, chat: "any;-;+15550000001", cancel: cancel}
}

func (h *harness) submit(r send.Request) *store.Send {
	h.t.Helper()
	if r.APIKey == "" {
		r.APIKey = "test"
	}
	snd, created, err := h.sender.Submit(context.Background(), r)
	if err != nil || !created {
		h.t.Fatalf("submit: %v created=%v", err, created)
	}
	return snd
}

func (h *harness) wait(id string) *store.Send {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snd, err := h.sender.Wait(ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	if !send.Settled(snd) {
		h.t.Fatalf("send did not settle: %+v", snd)
	}
	return snd
}

// eventually polls the send until cond holds.
func (h *harness) eventually(id string, cond func(*store.Send) bool) *store.Send {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		snd, _ := h.sender.Get(context.Background(), id)
		if cond(snd) {
			return snd
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("condition not met: %+v", snd)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTextToChat(t *testing.T) {
	h := newHarness(t)
	// send.updated is owned by the sending key; other keys don't see it.
	other, _, _ := h.bus.Subscribe(events.Filter{Types: []string{events.SendUpdated}, Owner: "someone-else"}, "", 100)
	sub, _, _ := h.bus.Subscribe(events.Filter{Types: []string{events.SendUpdated}, Owner: "test"}, "", 100)
	snd := h.submit(send.Request{ChatID: h.chat, Text: "hello there"})
	got := h.wait(snd.ID)
	it := got.Items[0]
	if got.Status != store.SendSent || it.MessageID == "" || it.Status != store.SendSent {
		t.Fatalf("send = %+v", got)
	}

	// Delivery receipt flows through to the send.
	h.f.DB.Exec(`UPDATE message SET is_delivered = 1 WHERE guid = ?`, it.MessageID)
	h.eventually(snd.ID, func(s *store.Send) bool { return s.Status == store.SendDelivered })

	// Persisted, and send.updated events were published.
	stored, _ := h.store.GetSend(context.Background(), snd.ID)
	if stored.Status != store.SendDelivered {
		t.Fatalf("stored status = %s", stored.Status)
	}
	var statuses []string
	for len(sub.C()) > 0 {
		ev := <-sub.C()
		if ev.ChatID != h.chat {
			t.Fatalf("event chat = %q", ev.ChatID)
		}
		statuses = append(statuses, string(ev.Data))
	}
	if len(statuses) < 2 || !strings.Contains(statuses[len(statuses)-1], `"status":"delivered"`) {
		t.Fatalf("send.updated events = %v", statuses)
	}
	if len(other.C()) != 0 {
		t.Fatal("another key received send.updated")
	}
}

func TestTextAndAttachmentsToAddress(t *testing.T) {
	h := newHarness(t)
	// Formatting differs from the stored "+15550000001"; matching uses digits.
	snd := h.submit(send.Request{To: "(555) 000-0001", Service: send.ServiceIMessage, Text: "pics",
		Files: []send.StagedFile{{Name: "a.jpg", Path: "/staged/0/a.jpg"}, {Name: "b.jpg", Path: "/staged/1/b.jpg"}}})
	got := h.wait(snd.ID)
	if got.Status != store.SendSent || len(got.Items) != 3 || got.ChatID != h.chat {
		t.Fatalf("send = %+v", got)
	}
	seen := map[string]bool{}
	for _, it := range got.Items {
		if it.MessageID == "" || seen[it.MessageID] {
			t.Fatalf("items = %+v", got.Items)
		}
		seen[it.MessageID] = true
	}
	if n := h.runner.CallCount(); n != 3 {
		t.Fatalf("runner calls = %d", n)
	}
}

func TestNewAddressCreatesChat(t *testing.T) {
	h := newHarness(t)
	snd := h.submit(send.Request{To: "New@Example.com", Text: "hi"})
	got := h.wait(snd.ID)
	if got.Status != store.SendSent || got.ChatID == "" {
		t.Fatalf("send = %+v", got)
	}
}

func TestAppleScriptFailure(t *testing.T) {
	h := newHarness(t)
	h.runner.FailNext(errors.New("Can’t get chat id"))
	snd := h.submit(send.Request{ChatID: h.chat, Text: "x", Files: []send.StagedFile{{Path: "/s/a.jpg"}}})
	got := h.wait(snd.ID)
	if got.Status != store.SendFailed || !strings.Contains(got.Items[0].Error, "Can’t get chat id") ||
		got.Items[1].Status != store.SendFailed || h.runner.CallCount() != 1 {
		t.Fatalf("send = %+v calls=%d", got, h.runner.CallCount())
	}
}

func TestMessagesError(t *testing.T) {
	h := newHarness(t)
	h.runner.ErrorNext(22)
	snd := h.submit(send.Request{To: "+15559999999", Text: "nobody"})
	got := h.wait(snd.ID)
	if got.Status != store.SendFailed || got.Items[0].MessageID == "" || !strings.Contains(got.Error, "22") {
		t.Fatalf("send = %+v", got)
	}
}

func TestTimeout(t *testing.T) {
	h := newHarness(t, func(o *send.Options) { o.TextTimeout = 100 * time.Millisecond })
	h.runner.DropNext()
	snd := h.submit(send.Request{ChatID: h.chat, Text: "lost"})
	got := h.wait(snd.ID)
	if got.Status != store.SendFailed || !strings.Contains(got.Items[0].Error, "did not appear") {
		t.Fatalf("send = %+v", got)
	}
}

func TestDuplicateTextsMatchedInOrder(t *testing.T) {
	h := newHarness(t)
	a := h.submit(send.Request{ChatID: h.chat, Text: "same"})
	b := h.submit(send.Request{ChatID: h.chat, Text: "same"})
	ga, gb := h.wait(a.ID), h.wait(b.ID)
	if ga.Items[0].MessageID == gb.Items[0].MessageID || ga.Items[0].MessageRowID > gb.Items[0].MessageRowID {
		t.Fatalf("a=%+v b=%+v", ga.Items[0], gb.Items[0])
	}
}

func TestIgnoresMessagesFromOtherDevices(t *testing.T) {
	h := newHarness(t)
	h.runner.DropNext()
	snd := h.submit(send.Request{ChatID: h.chat, Text: "mine"})
	// A from-me message with different text (e.g. typed on the phone).
	h.f.Message(fixture.Msg{Chat: 1, FromMe: true, Text: "something else", Date: time.Now(), IsSent: true})
	time.Sleep(100 * time.Millisecond)
	got, _ := h.sender.Get(context.Background(), snd.ID)
	if got.Items[0].MessageID != "" {
		t.Fatalf("matched the wrong message: %+v", got.Items[0])
	}
}

func TestIdempotency(t *testing.T) {
	h := newHarness(t)
	first := h.submit(send.Request{ChatID: h.chat, Text: "once", ClientRef: "abc"})
	again, created, err := h.sender.Submit(context.Background(), send.Request{APIKey: "test", ChatID: h.chat, Text: "once", ClientRef: "abc"})
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("again = %+v created=%v err=%v", again, created, err)
	}
	h.wait(first.ID)
	if n := h.runner.CallCount(); n != 1 {
		t.Fatalf("sent %d times", n)
	}
}

func TestRecoverAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "imsgd.db")
	st, _ := store.Open(path)
	st.CreateSend(context.Background(), &store.Send{ID: "snd_old", APIKey: "k", ChatID: "any;-;x", Status: store.SendPending,
		Items: []store.SendItem{
			{Index: 0, Kind: "text", Text: "a", Status: store.SendSent, MessageID: "GUID-A"},
			{Index: 1, Kind: "text", Text: "b", Status: store.SendPending},
		}})
	sender := send.New(send.Options{Store: st, StagingDir: t.TempDir(), Runner: &sendtest.Runner{}})
	ctx, cancel := context.WithCancel(context.Background())
	go sender.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	got, _ := sender.Get(context.Background(), "snd_old")
	cancel()
	if got.Status != store.SendFailed || got.Items[0].Status != store.SendSent || !strings.Contains(got.Items[1].Error, "restarted") {
		t.Fatalf("recovered = %+v", got)
	}
	// The matched item is still tracked.
	sender.OnMessageUpdated(&db.Message{GUID: "GUID-A", IsDelivered: true}, nil)
	got, _ = sender.Get(context.Background(), "snd_old")
	if got.Items[0].Status != store.SendDelivered {
		t.Fatalf("matched item not tracked: %+v", got.Items[0])
	}
}

func TestValidAddressAndFilenames(t *testing.T) {
	for _, ok := range []string{"+14165550100", "(416) 555-0100", "416.555.0100", "a.b@example.co.uk"} {
		if !send.ValidAddress(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	for _, bad := range []string{"", "hello", "12", "a@b", "+1 416 555 0100; rm -rf"} {
		if send.ValidAddress(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	cases := map[string]string{
		"../../etc/passwd":  "passwd",
		"a/b\\c.jpg":        "c.jpg",
		".hidden":           "hidden",
		"":                  "attachment",
		"..":                "attachment",
		"ok name.png":       "ok name.png",
		"bad\x00\nname.gif": "badname.gif",
	}
	for in, want := range cases {
		if got := send.SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeLongNames(t *testing.T) {
	long := "a." + strings.Repeat("b", 250) // previously panicked
	if got := send.SanitizeFilename(long); len(got) > 200 {
		t.Fatalf("len %d", len(got))
	}
	emoji := strings.Repeat("😀", 80) + ".jpg" // multi-byte: must not split a rune
	got := send.SanitizeFilename(emoji)
	if len(got) > 200 || !strings.HasSuffix(got, ".jpg") || !utf8.ValidString(got) {
		t.Fatalf("got %q", got)
	}
}
