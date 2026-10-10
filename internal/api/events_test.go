package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/matthewmgamble/imsgd/internal/events"
	"github.com/matthewmgamble/imsgd/internal/webhook"
)

func (e *env) dial(query string, header http.Header) (*websocket.Conn, *http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(e.ts.URL, "http") + "/api/v1/events" + query
	return websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
}

func authHeader() http.Header {
	return http.Header{"Authorization": {"Bearer " + testKey}}
}

func readFrame(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func TestWebSocketStream(t *testing.T) {
	e := newEnv(t)
	c, _, err := e.dial("", authHeader())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	hello := readFrame(t, c)
	if hello["type"] != "hello" || hello["data"].(map[string]any)["resumed"] != true {
		t.Fatalf("hello = %v", hello)
	}

	ev, _ := e.bus.Publish(events.MessageReceived, "chat-1", map[string]string{"x": "1"})
	got := readFrame(t, c)
	if got["id"] != ev.ID || got["type"] != events.MessageReceived || got["data"].(map[string]any)["x"] != "1" {
		t.Fatalf("event = %v", got)
	}
	if _, ok := got["ChatID"]; ok {
		t.Fatal("internal field leaked")
	}

	// Resume after ev: the two later events are replayed after hello.
	e.bus.Publish(events.ChatRead, "chat-1", map[string]string{"n": "2"})
	e3, _ := e.bus.Publish(events.MessageSent, "chat-2", map[string]string{"n": "3"})
	c2, _, err := e.dial("?last_event_id="+ev.ID, authHeader())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.CloseNow()
	h2 := readFrame(t, c2)["data"].(map[string]any)
	if h2["resumed"] != true || h2["replayed"].(float64) != 2 || h2["last_event_id"] != e3.ID {
		t.Fatalf("resume hello = %v", h2)
	}
	if readFrame(t, c2)["type"] != events.ChatRead || readFrame(t, c2)["id"] != e3.ID {
		t.Fatal("replay order wrong")
	}

	// Unknown last_event_id → resumed false.
	c3, _, _ := e.dial("?last_event_id=ffffffff-1", authHeader())
	defer c3.CloseNow()
	if readFrame(t, c3)["data"].(map[string]any)["resumed"] != false {
		t.Fatal("expected resumed=false")
	}
}

func TestWebSocketFiltersAndAuth(t *testing.T) {
	e := newEnv(t)
	// Token in query string (browser style), filtered by type and chat.
	c, _, err := e.dial("?token="+testKey+"&types=message.*&chat_id=chat-2&chat_id=chat-3", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	readFrame(t, c)                                    // hello
	e.bus.Publish(events.MessageReceived, "chat-1", 1) // wrong chat
	e.bus.Publish(events.ChatRead, "chat-2", 2)        // wrong type
	want, _ := e.bus.Publish(events.MessageUpdated, "chat-3", 3)
	if got := readFrame(t, c); got["id"] != want.ID {
		t.Fatalf("filtered stream got %v", got)
	}

	for name, h := range map[string]http.Header{"none": nil, "bad": {"Authorization": {"Bearer nope-nope-nope-nope"}}} {
		_, res, err := e.dial("", h)
		if err == nil || res == nil || res.StatusCode != 401 {
			t.Errorf("%s auth: err=%v res=%v", name, err, res)
		}
	}
	if _, res, err := e.dial("?types=bogus", authHeader()); err == nil || res.StatusCode != 400 {
		t.Errorf("bad types accepted: %v", err)
	}
	// Plain GET without upgrade is a documented 400.
	wantStatus(t, e.get("/api/v1/events"), 400, "bad_request")
}

func TestWebSocketShutdown(t *testing.T) {
	e := newEnv(t)
	c, _, err := e.dial("", authHeader())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	readFrame(t, c)
	e.srv.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err = c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("close status = %v (%v)", websocket.CloseStatus(err), err)
	}
}

func TestWebhookAPI(t *testing.T) {
	e := newEnv(t)
	var hits int
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer recv.Close()

	// Validation.
	wantStatus(t, e.doBody("POST", "/api/v1/webhooks", map[string]any{"url": "ftp://x"}), 400, "bad_request")
	wantStatus(t, e.doBody("POST", "/api/v1/webhooks", map[string]any{"url": recv.URL, "events": []string{"nope"}}), 400, "bad_request")
	wantStatus(t, e.doBody("POST", "/api/v1/webhooks", map[string]any{"url": recv.URL, "bogus": 1}), 400, "bad_request")
	wantStatus(t, e.doBody("POST", "/api/v1/webhooks", "{not json"), 400, "bad_request")

	created := e.doBody("POST", "/api/v1/webhooks", map[string]any{"url": recv.URL, "events": []string{"message.*"}, "description": "test"})
	wantStatus(t, created, 201, "")
	w := created.json(t)
	id := w["id"].(string)
	secret, _ := w["secret"].(string)
	if !strings.HasPrefix(secret, "whsec_") || w["enabled"] != true {
		t.Fatalf("created = %v", w)
	}

	got := e.get("/api/v1/webhooks/" + id).json(t)
	if _, ok := got["secret"]; ok {
		t.Fatal("secret returned by GET")
	}
	if list := e.get("/api/v1/webhooks").json(t)["data"].([]any); len(list) != 1 {
		t.Fatalf("list = %v", list)
	}

	up := e.doBody("PATCH", "/api/v1/webhooks/"+id, map[string]any{"enabled": false, "chat_ids": []string{"any;-;x"}})
	wantStatus(t, up, 200, "")
	if m := up.json(t); m["enabled"] != false || m["description"] != "test" || len(m["chat_ids"].([]any)) != 1 {
		t.Fatalf("patched = %v", m)
	}

	test := e.do("POST", "/api/v1/webhooks/"+id+"/test")
	wantStatus(t, test, 200, "")
	if m := test.json(t); m["ok"] != true || m["status_code"].(float64) != 200 || hits != 1 {
		t.Fatalf("test = %v hits=%d", m, hits)
	}

	e.store.EnqueueDelivery(context.Background(), id, "evt-1", "message.sent", []byte(`{}`))
	dl := e.get("/api/v1/webhooks/" + id + "/deliveries").json(t)["data"].([]any)
	if len(dl) != 1 || dl[0].(map[string]any)["status"] != "pending" {
		t.Fatalf("deliveries = %v", dl)
	}

	wantStatus(t, e.do("DELETE", "/api/v1/webhooks/"+id), 204, "")
	wantStatus(t, e.get("/api/v1/webhooks/"+id), 404, "not_found")
	wantStatus(t, e.do("DELETE", "/api/v1/webhooks/"+id), 404, "not_found")
	_ = webhook.HeaderSignature
}

func TestWebhookOwnership(t *testing.T) {
	e := newEnv(t)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer recv.Close()
	created := e.doBody("POST", "/api/v1/webhooks", map[string]any{"url": recv.URL})
	wantStatus(t, created, 201, "")
	id := created.json(t)["id"].(string)
	secret := created.json(t)["secret"].(string)

	// Another key can't see, change, test or delete it.
	if list := e.get("/api/v1/webhooks", otherKeyAuth).json(t)["data"].([]any); len(list) != 0 {
		t.Fatalf("other key sees %v", list)
	}
	for _, r := range []resp{
		e.get("/api/v1/webhooks/"+id, otherKeyAuth),
		e.doBody("PATCH", "/api/v1/webhooks/"+id, map[string]any{"url": "http://attacker.example/"}, otherKeyAuth),
		e.do("POST", "/api/v1/webhooks/"+id+"/test", otherKeyAuth),
		e.get("/api/v1/webhooks/"+id+"/deliveries", otherKeyAuth),
		e.do("DELETE", "/api/v1/webhooks/"+id, otherKeyAuth),
	} {
		wantStatus(t, r, 404, "not_found")
	}

	// The owner changing the URL rotates the secret and returns the new one.
	up := e.doBody("PATCH", "/api/v1/webhooks/"+id, map[string]any{"url": recv.URL + "/v2"})
	wantStatus(t, up, 200, "")
	newSecret, _ := up.json(t)["secret"].(string)
	if newSecret == "" || newSecret == secret {
		t.Fatalf("secret not rotated: %v", up.json(t))
	}
	// Other edits don't expose the secret.
	if _, ok := e.doBody("PATCH", "/api/v1/webhooks/"+id, map[string]any{"description": "x"}).json(t)["secret"]; ok {
		t.Fatal("secret returned without a URL change")
	}
}
