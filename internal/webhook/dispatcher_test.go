package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matthewmgamble/imsgd/internal/events"
	"github.com/matthewmgamble/imsgd/internal/store"
)

type receiver struct {
	mu       sync.Mutex
	bodies   [][]byte
	headers  []http.Header
	failNext atomic.Int32
	got      chan struct{}
}

func newReceiver(t *testing.T) (*receiver, *httptest.Server) {
	r := &receiver{got: make(chan struct{}, 100)}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.failNext.Load() > 0 {
			r.failNext.Add(-1)
			w.WriteHeader(503)
			r.got <- struct{}{}
			return
		}
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, b)
		r.headers = append(r.headers, req.Header.Clone())
		r.mu.Unlock()
		r.got <- struct{}{}
	}))
	t.Cleanup(ts.Close)
	return r, ts
}

func (r *receiver) wait(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-r.got:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for request %d", i+1)
		}
	}
}

func setup(t *testing.T) (*Dispatcher, *store.Store, *events.Bus) {
	st, err := store.Open(filepath.Join(t.TempDir(), "imsgd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	bus := events.NewBus(100)
	return New(Options{Store: st, Bus: bus, Version: "test"}), st, bus
}

func TestDeliverAndSign(t *testing.T) {
	d, st, bus := setup(t)
	rec, ts := newReceiver(t)
	all := &store.Webhook{URL: ts.URL, Enabled: true}
	st.CreateWebhook(context.Background(), all)
	onlyRead := &store.Webhook{URL: ts.URL + "/read", Events: []string{"chat.read"}, Enabled: true}
	st.CreateWebhook(context.Background(), onlyRead)
	disabled := &store.Webhook{URL: ts.URL + "/off", Enabled: false}
	st.CreateWebhook(context.Background(), disabled)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond) // subscribe

	ev, _ := bus.Publish(events.MessageReceived, "c1", map[string]string{"x": "y"})
	rec.wait(t, 1)
	bus.Publish(events.ChatRead, "c1", map[string]string{"chat_id": "c1"})
	rec.wait(t, 2) // both "all" and "onlyRead"

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.bodies) != 3 {
		t.Fatalf("got %d deliveries", len(rec.bodies))
	}
	var got events.Event
	json.Unmarshal(rec.bodies[0], &got)
	if got.ID != ev.ID || got.Type != events.MessageReceived || string(got.Data) != `{"x":"y"}` {
		t.Fatalf("payload = %s", rec.bodies[0])
	}
	h := rec.headers[0]
	if h.Get(HeaderEvent) != events.MessageReceived || h.Get(HeaderEventID) != ev.ID || h.Get(HeaderDelivery) == "" {
		t.Fatalf("headers = %v", h)
	}
	if err := Verify(all.Secret, h.Get(HeaderSignature), rec.bodies[0], time.Now(), 5*time.Minute); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if err := Verify(onlyRead.Secret, h.Get(HeaderSignature), rec.bodies[0], time.Now(), 5*time.Minute); err == nil {
		t.Fatal("signature verified with wrong secret")
	}
}

func TestRetryThenSucceed(t *testing.T) {
	d, st, bus := setup(t)
	rec, ts := newReceiver(t)
	rec.failNext.Store(2)
	retryDelays = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond, time.Hour}
	MaxAttempts = 4
	defer func() {
		retryDelays = []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}
		MaxAttempts = len(retryDelays) + 1
	}()
	w := &store.Webhook{URL: ts.URL, Enabled: true}
	st.CreateWebhook(context.Background(), w)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	bus.Publish(events.MessageSent, "c", 1)
	rec.wait(t, 3) // 503, 503, 200

	deadline := time.Now().Add(2 * time.Second)
	for {
		list, _ := st.ListDeliveries(context.Background(), w.ID, 10)
		if len(list) == 1 && list[0].Status == store.DeliverySucceeded {
			if list[0].Attempts != 3 {
				t.Fatalf("attempts = %d", list[0].Attempts)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery = %+v", list[0])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestGiveUp(t *testing.T) {
	d, st, bus := setup(t)
	rec, ts := newReceiver(t)
	rec.failNext.Store(100)
	retryDelays = []time.Duration{5 * time.Millisecond}
	MaxAttempts = 2
	defer func() {
		retryDelays = []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}
		MaxAttempts = len(retryDelays) + 1
	}()
	w := &store.Webhook{URL: ts.URL, Enabled: true}
	st.CreateWebhook(context.Background(), w)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	bus.Publish(events.MessageSent, "c", 1)
	rec.wait(t, 2)
	time.Sleep(100 * time.Millisecond)
	list, _ := st.ListDeliveries(context.Background(), w.ID, 10)
	if list[0].Status != store.DeliveryFailed || list[0].Attempts != 2 || *list[0].LastStatusCode != 503 {
		t.Fatalf("delivery = %+v", list[0])
	}
}

func TestDeliveriesSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "imsgd.db")
	st, _ := store.Open(path)
	rec, ts := newReceiver(t)
	w := &store.Webhook{URL: ts.URL, Enabled: true}
	st.CreateWebhook(context.Background(), w)
	// Queued but never sent (e.g. crash before delivery).
	st.EnqueueDelivery(context.Background(), w.ID, "evt-x", "message.sent", []byte(`{"id":"evt-x"}`))
	st.Close()

	st2, _ := store.Open(path)
	defer st2.Close()
	d := New(Options{Store: st2, Bus: events.NewBus(10), Version: "test"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	rec.wait(t, 1)
}

func TestSendTestAndVerify(t *testing.T) {
	d, st, _ := setup(t)
	rec, ts := newReceiver(t)
	w := &store.Webhook{URL: ts.URL, Enabled: true}
	st.CreateWebhook(context.Background(), w)
	r := d.SendTest(context.Background(), w)
	if !r.OK || r.StatusCode != 200 {
		t.Fatalf("test result = %+v", r)
	}
	rec.wait(t, 1)

	bad := d.SendTest(context.Background(), &store.Webhook{URL: "http://127.0.0.1:1/nope", Secret: "s"})
	if bad.OK || bad.Error == "" {
		t.Fatalf("expected failure: %+v", bad)
	}

	body := []byte(`{}`)
	now := time.Unix(1700000000, 0)
	h := Sign("sec", now.Unix(), body)
	if err := Verify("sec", h, body, now.Add(time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if Verify("sec", h, body, now.Add(time.Hour), 5*time.Minute) == nil {
		t.Fatal("stale signature accepted")
	}
	if Verify("sec", h, []byte(`{"x":1}`), now, 5*time.Minute) == nil {
		t.Fatal("tampered body accepted")
	}
}

func TestRevokedOwnerNotDelivered(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "imsgd.db"))
	defer st.Close()
	bus := events.NewBus(10)
	rec, ts := newReceiver(t)
	live := map[string]bool{"alive": true}
	d := New(Options{Store: st, Bus: bus, Version: "test", KeyExists: func(n string) bool { return live[n] }})
	st.CreateWebhook(context.Background(), &store.Webhook{Owner: "revoked", URL: ts.URL + "/a", Enabled: true})
	st.CreateWebhook(context.Background(), &store.Webhook{Owner: "alive", URL: ts.URL + "/b", Enabled: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	bus.Publish(events.MessageReceived, "c", 1)
	// An event owned by another key must not reach "alive"'s webhook.
	bus.PublishOwned(events.SendUpdated, "c", "someone-else", 2)
	rec.wait(t, 1)
	time.Sleep(200 * time.Millisecond)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.bodies) != 1 || !strings.Contains(string(rec.bodies[0]), events.MessageReceived) {
		t.Fatalf("deliveries = %d", len(rec.bodies))
	}
}

func TestNoRedirectFollow(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	d, st, _ := setup(t)
	w := &store.Webhook{URL: redirect.URL, Enabled: true}
	st.CreateWebhook(context.Background(), w)
	if r := d.SendTest(context.Background(), w); r.OK || r.StatusCode != 307 {
		t.Fatalf("result = %+v", r)
	}
	if hits.Load() != 0 {
		t.Fatal("redirect was followed")
	}
	bad := d.SendTest(context.Background(), &store.Webhook{URL: "http://127.0.0.1:1/secret-path", Secret: "s"})
	if strings.Contains(bad.Error, "secret-path") || strings.Contains(bad.Error, "127.0.0.1") {
		t.Fatalf("error leaks URL: %q", bad.Error)
	}
}
