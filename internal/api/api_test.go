package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"howett.net/plist"

	apispec "github.com/matthewmgamble/imsgd/api"
	"github.com/matthewmgamble/imsgd/internal/config"
	"github.com/matthewmgamble/imsgd/internal/events"
	"github.com/matthewmgamble/imsgd/internal/imessage/db"
	"github.com/matthewmgamble/imsgd/internal/imessage/fixture"
	"github.com/matthewmgamble/imsgd/internal/imessage/typedstream"
	"github.com/matthewmgamble/imsgd/internal/media"
	"github.com/matthewmgamble/imsgd/internal/present"
	"github.com/matthewmgamble/imsgd/internal/send"
	"github.com/matthewmgamble/imsgd/internal/send/sendtest"
	"github.com/matthewmgamble/imsgd/internal/store"
	"github.com/matthewmgamble/imsgd/internal/tracker"
	"github.com/matthewmgamble/imsgd/internal/webhook"
)

const testKey = "test-key-0123456789abcdef"

func at(min int) time.Time { return time.Date(2026, 3, 1, 10, min, 0, 0, time.UTC) }

type env struct {
	t      *testing.T
	ts     *httptest.Server
	router routers.Router
	ids    map[string]string
	root   string
	bus    *events.Bus
	store  *store.Store
	srv    *Server
	runner *sendtest.Runner
	f      *fixture.Builder
}

func TestMain(m *testing.M) {
	if os.Getenv("IMSGD_TEST_LOG") == "" {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	os.Exit(m.Run())
}

func init() {
	openapi3filter.RegisterBodyDecoder("application/yaml", func(r io.Reader, _ http.Header, _ *openapi3.SchemaRef, _ openapi3filter.EncodingFn) (any, error) {
		b, err := io.ReadAll(r)
		return string(b), err
	})
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := fixture.New(t)
	root := t.TempDir()
	ids := map[string]string{}

	alice := f.Handle("+15550000001", "iMessage")
	bob := f.Handle("+15550000002", "iMessage")
	dm := f.Chat("any;-;+15550000001", fixture.StyleDirect, "", alice)
	group := f.Chat("any;+;chat0001", fixture.StyleGroup, "Climbing", bob, alice)
	f.SetLastRead(dm, at(2))

	_, ids["hello"] = f.Message(fixture.Msg{Chat: dm, Handle: alice, Text: "hello", Date: at(1), IsDelivered: true})
	_, ids["reply"] = f.Message(fixture.Msg{Chat: dm, Handle: alice, FromMe: true, Text: "hi!", Date: at(2), IsSent: true, IsDelivered: true, Delivered: at(2), Read: at(3)})
	_, ids["love"] = f.Message(fixture.Msg{Chat: dm, Handle: alice, Text: "Loved “hi!”", Date: at(3), AssociatedGUID: "p:0/" + ids["reply"], AssociatedType: 2000})

	summary, _ := plist.Marshal(map[string]any{"ep": []any{0}, "ec": map[string]any{"0": []any{map[string]any{"d": 8.1e8, "t": typedstream.EncodeAttributedString("helo", nil)}}}}, plist.BinaryFormat)
	_, ids["edited"] = f.Message(fixture.Msg{Chat: dm, Handle: alice, FromMe: true, Text: "hello", Date: at(4), Edited: at(5), SummaryInfo: summary, PartCount: 1})
	unsentSummary, _ := plist.Marshal(map[string]any{"rp": []any{0}}, plist.BinaryFormat)
	_, ids["unsent"] = f.Message(fixture.Msg{Chat: dm, Handle: alice, FromMe: true, Date: at(6), Edited: at(7), SummaryInfo: unsentSummary})

	// Group: photo on disk, a missing file, and a hidden link-preview payload.
	photoPath := filepath.Join(root, "IMG_1.jpg")
	if err := os.WriteFile(photoPath, bytes.Repeat([]byte("JPEGDATA"), 128), 0o600); err != nil {
		t.Fatal(err)
	}
	photoMsg, photo := f.Message(fixture.Msg{Chat: group, Handle: bob, Text: "look ￼", Date: at(8), Runs: []typedstream.Run{
		{Length: 5, Attributes: map[string]any{"__kIMMessagePartAttributeName": 0}},
		{Length: 1, Attributes: map[string]any{"__kIMMessagePartAttributeName": 1, "__kIMFileTransferGUIDAttributeName": "ATT-PHOTO"}},
	}})
	ids["photo"] = photo
	f.Attachment(photoMsg, fixture.Att{GUID: "ATT-PHOTO", Filename: photoPath, MimeType: "image/jpeg", UTI: "public.jpeg", TransferName: "IMG_1.jpg", TotalBytes: 1024})
	missingMsg, _ := f.Message(fixture.Msg{Chat: group, Handle: bob, Text: "￼", Date: at(9)})
	f.Attachment(missingMsg, fixture.Att{GUID: "ATT-MISSING", Filename: filepath.Join(root, "gone.mov"), MimeType: "video/quicktime", TransferName: "gone.mov", TransferState: 0})
	f.Attachment(missingMsg, fixture.Att{GUID: "ATT-OUTSIDE", Filename: "/etc/hosts", TransferName: "hosts"})
	linkMsg, _ := f.Message(fixture.Msg{Chat: group, Handle: alice, Text: "https://example.com", Date: at(10), BalloonBundleID: "com.apple.messages.URLBalloonProvider"})
	f.Attachment(linkMsg, fixture.Att{GUID: "ATT-HIDDEN", Filename: filepath.Join(root, "x.pluginPayloadAttachment"), TransferName: "x.pluginPayloadAttachment"})
	f.DB.Exec(`UPDATE attachment SET hide_attachment = 1 WHERE guid = 'ATT-HIDDEN'`)

	// A real PNG with a Live Photo companion in its own attachment directory.
	pngDir := filepath.Join(root, "png-att")
	os.MkdirAll(pngDir, 0o700)
	var pngBuf bytes.Buffer
	png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 640, 480)))
	os.WriteFile(filepath.Join(pngDir, "pic.png"), pngBuf.Bytes(), 0o600)
	os.WriteFile(filepath.Join(pngDir, "pic.mov"), []byte("MOVDATA"), 0o600)
	pngMsg, _ := f.Message(fixture.Msg{Chat: group, Handle: bob, Text: "\uFFFC", Date: at(10).Add(30 * time.Second)})
	f.Attachment(pngMsg, fixture.Att{GUID: "ATT-PNG", Filename: filepath.Join(pngDir, "pic.png"), MimeType: "image/png", TransferName: "pic.png"})

	f.Message(fixture.Msg{Chat: group, Handle: bob, ItemType: 1, OtherHandle: alice, Date: at(11)})
	_, ids["emoji"] = f.Message(fixture.Msg{Chat: group, Handle: alice, Date: at(12), AssociatedGUID: "p:1/" + photo, AssociatedType: 2006, AssociatedEmoji: "🔥"})

	d, err := db.Open(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	bus := events.NewBus(5)
	st, err := store.Open(filepath.Join(t.TempDir(), "imsgd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	runner := &sendtest.Runner{F: f}
	sender := send.New(send.Options{
		Store: st, Bus: bus, Runner: runner, StagingDir: filepath.Join(root, "staging"),
		Present: func(s *store.Send) any { return present.SendOf(s) }, SweepInterval: 20 * time.Millisecond,
		TextTimeout: 300 * time.Millisecond,
	})
	trk := tracker.New(tracker.Options{
		DB: d, Bus: bus, Presenter: &present.Presenter{DB: d},
		OnNewMessage: sender.OnNewMessage, OnMessageUpdated: sender.OnMessageUpdated,
	})
	if err := trk.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	bg, stop := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	// Stop background workers before the fixture DB is closed (cleanups run
	// in reverse order of registration).
	t.Cleanup(func() { stop(); workers.Wait() })
	workers.Add(2)
	go func() { defer workers.Done(); sender.Run(bg) }()
	go func() {
		defer workers.Done()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-bg.Done():
				return
			case <-tick.C:
				trk.Tick(bg)
			}
		}
	}()
	srv := New(Options{
		Converter:       media.NewConverter(filepath.Join(root, "cache"), 1<<30),
		Dims:            media.NewDimensionCache(100),
		Sender:          sender,
		UploadMaxBytes:  1 << 20,
		Bus:             bus,
		Store:           st,
		Webhooks:        webhook.New(webhook.Options{Store: st, Bus: bus, Version: "test"}),
		DB:              d,
		APIKeys:         []config.APIKey{{Name: "test", Key: testKey}, {Name: "other", Key: "other-key-0123456789abcd"}},
		Version:         "test",
		MacOSVersion:    "26.5.2",
		AttachmentRoots: []string{root},
		Names: func(addr string) *string {
			if addr == "+15550000001" {
				n := "Alice"
				return &n
			}
			return nil
		},
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatal(err)
	}
	doc.Servers = openapi3.Servers{{URL: ts.URL}}
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, ts: ts, router: router, ids: ids, root: root, bus: bus, store: st, srv: srv, runner: runner, f: f}
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, r.body)
	}
	return m
}

// get performs a request and validates request and response against the
// OpenAPI spec. Unknown routes are only checked for a JSON error body.
func (e *env) get(path string, opts ...func(*http.Request)) resp {
	e.t.Helper()
	return e.do("GET", path, opts...)
}

func (e *env) do(method, path string, opts ...func(*http.Request)) resp {
	e.t.Helper()
	return e.doBody(method, path, nil, opts...)
}

// doBody sends a JSON body (nil for none).
func (e *env) doBody(method, path string, body any, opts ...func(*http.Request)) resp {
	e.t.Helper()
	var rd io.Reader
	var raw []byte
	if body != nil {
		if b, ok := body.(string); ok {
			raw = []byte(b)
		} else {
			raw, _ = json.Marshal(body)
		}
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	for _, o := range opts {
		o(req)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resBody, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if body != nil { // the request body was consumed; restore it for validation
		req.Body = io.NopCloser(bytes.NewReader(raw))
	}
	route, params, err := e.router.FindRoute(req)
	if err != nil {
		if !strings.Contains(res.Header.Get("Content-Type"), "application/json") {
			e.t.Errorf("%s %s: non-JSON response for unknown route", method, path)
		}
		return resp{res.StatusCode, res.Header, resBody}
	}
	in := &openapi3filter.RequestValidationInput{
		Request: req, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}
	ctx := context.Background()
	reqErr := openapi3filter.ValidateRequest(ctx, in)
	if err := openapi3filter.ValidateResponse(ctx, &openapi3filter.ResponseValidationInput{
		RequestValidationInput: in,
		Status:                 res.StatusCode,
		Header:                 res.Header,
		Body:                   io.NopCloser(bytes.NewReader(resBody)),
		Options:                &openapi3filter.Options{IncludeResponseStatus: true},
	}); err != nil {
		e.t.Errorf("%s %s: response does not match spec: %v\n%s", method, path, err, truncate(resBody))
	}
	// Requests the spec rejects must also be rejected by the server.
	if reqErr != nil && res.StatusCode < 400 {
		e.t.Errorf("%s %s: spec rejects request (%v) but server returned %d", method, path, reqErr, res.StatusCode)
	}
	return resp{res.StatusCode, res.Header, resBody}
}

func truncate(b []byte) string {
	if len(b) > 2000 {
		return string(b[:2000]) + "…"
	}
	return string(b)
}

func noAuth(r *http.Request)       { r.Header.Del("Authorization") }
func badAuth(r *http.Request)      { r.Header.Set("Authorization", "Bearer wrong-key-000000000000") }
func basicAuth(r *http.Request)    { r.SetBasicAuth("u", testKey) }
func otherKeyAuth(r *http.Request) { r.Header.Set("Authorization", "bearer other-key-0123456789abcd") }

func wantStatus(t *testing.T, r resp, status int, code string) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d\n%s", r.status, status, r.body)
	}
	if code != "" {
		e, _ := r.json(t)["error"].(map[string]any)
		if e["code"] != code {
			t.Fatalf("error code = %v, want %s", e["code"], code)
		}
	}
}

// ---- tests -------------------------------------------------------------------

func TestRoutesMatchSpec(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromData(apispec.Spec)
	if err != nil {
		t.Fatal(err)
	}
	implemented := map[string]bool{}
	for _, rt := range routes {
		key := rt.Method + " " + rt.Pattern
		implemented[key] = true
		item := doc.Paths.Find(rt.Pattern)
		if item == nil || item.GetOperation(rt.Method) == nil {
			t.Errorf("route %s is not documented in openapi.yaml", key)
			continue
		}
		if op := item.GetOperation(rt.Method); op != nil {
			public := op.Security != nil && len(*op.Security) == 0
			if public != rt.Public {
				t.Errorf("route %s: public=%v in code but %v in spec", key, rt.Public, public)
			}
		}
	}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			if !implemented[method+" "+path] {
				t.Errorf("spec documents %s %s but it is not implemented", method, path)
			}
		}
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	wantStatus(t, e.get("/health", noAuth), 200, "")
	wantStatus(t, e.get("/api/v1/chats", noAuth), 401, "unauthorized")
	wantStatus(t, e.get("/api/v1/chats", badAuth), 401, "unauthorized")
	wantStatus(t, e.get("/api/v1/chats", basicAuth), 401, "unauthorized")
	wantStatus(t, e.get("/api/v1/chats", otherKeyAuth), 200, "") // scheme is case-insensitive
	wantStatus(t, e.get("/api/v1/chats?token="+testKey, noAuth), 401, "unauthorized")
}

func TestErrors(t *testing.T) {
	e := newEnv(t)
	wantStatus(t, e.do("POST", "/api/v1/chats"), 405, "method_not_allowed")
	wantStatus(t, e.get("/api/v1/nope"), 404, "not_found")
	wantStatus(t, e.get("/api/v1/chats?limit=0"), 400, "bad_request")
	wantStatus(t, e.get("/api/v1/chats?limit=1001"), 400, "bad_request")
	wantStatus(t, e.get("/api/v1/chats?include=bogus"), 400, "bad_request")
	wantStatus(t, e.get("/api/v1/chats?cursor=garbage"), 400, "bad_request")
	wantStatus(t, e.get("/api/v1/messages?since_id=-1"), 400, "bad_request")
	wantStatus(t, e.get("/api/v1/chats/nope"), 404, "not_found")
	wantStatus(t, e.get("/api/v1/chats/nope/messages"), 404, "not_found")
	wantStatus(t, e.get("/api/v1/messages/nope"), 404, "not_found")
	wantStatus(t, e.get("/api/v1/attachments/nope"), 404, "not_found")
	// A handles cursor cannot be used for chats.
	h := e.get("/api/v1/handles?limit=1").json(t)
	wantStatus(t, e.get("/api/v1/chats?cursor="+h["next_cursor"].(string)), 400, "bad_request")
}

func TestServerInfo(t *testing.T) {
	e := newEnv(t)
	r := e.get("/api/v1/server")
	wantStatus(t, r, 200, "")
	m := r.json(t)
	if m["macos_version"] != "26.5.2" || m["messages_db"].(map[string]any)["max_rowid"].(float64) < 1 {
		t.Fatalf("server = %v", m)
	}
	spec := e.get("/api/v1/openapi.yaml")
	if spec.status != 200 || !bytes.Equal(spec.body, apispec.Spec) {
		t.Fatal("spec not served")
	}
}

func TestChats(t *testing.T) {
	e := newEnv(t)
	r := e.get("/api/v1/chats?include=participants,last_message")
	wantStatus(t, r, 200, "")
	m := r.json(t)
	data := m["data"].([]any)
	if len(data) != 2 || m["next_cursor"] != nil {
		t.Fatalf("chats = %s", r.body)
	}
	group := data[0].(map[string]any) // most recent activity
	if group["id"] != "any;+;chat0001" || group["display_name"] != "Climbing" || group["is_group"] != true {
		t.Fatalf("group = %v", group)
	}
	parts := group["participants"].([]any)
	if len(parts) != 2 || parts[1].(map[string]any)["name"] != "Alice" {
		t.Fatalf("participants = %v", parts)
	}
	if lm := group["last_message"].(map[string]any); lm["id"] != e.ids["emoji"] {
		t.Fatalf("last_message = %v", lm)
	}
	dm := data[1].(map[string]any)
	if dm["display_name"] != nil || dm["last_read_at"] != "2026-03-01T10:02:00Z" {
		t.Fatalf("dm = %v", dm)
	}

	// Without include, participants/last_message are absent.
	plain := e.get("/api/v1/chats").json(t)["data"].([]any)[0].(map[string]any)
	if _, ok := plain["participants"]; ok {
		t.Fatal("participants should be omitted")
	}

	// Paging one at a time visits both chats.
	var seen []string
	path := "/api/v1/chats?limit=1"
	for path != "" {
		m := e.get(path).json(t)
		for _, c := range m["data"].([]any) {
			seen = append(seen, c.(map[string]any)["id"].(string))
		}
		path = ""
		if nc, ok := m["next_cursor"].(string); ok {
			path = "/api/v1/chats?limit=1&cursor=" + nc
		}
	}
	if len(seen) != 2 || seen[0] == seen[1] {
		t.Fatalf("paged = %v", seen)
	}

	// Search and single chat.
	if n := len(e.get("/api/v1/chats?q=climb").json(t)["data"].([]any)); n != 1 {
		t.Fatalf("search returned %d", n)
	}
	one := e.get("/api/v1/chats/any;-;+15550000001")
	wantStatus(t, one, 200, "")
	if p := one.json(t)["participants"].([]any); len(p) != 1 {
		t.Fatalf("participants = %v", p)
	}
	// Percent-encoded IDs work too.
	wantStatus(t, e.get("/api/v1/chats/any%3B-%3B%2B15550000001"), 200, "")
}

func TestChatMessages(t *testing.T) {
	e := newEnv(t)
	collect := func(order string) []string {
		var ids []string
		path := "/api/v1/chats/any;-;+15550000001/messages?limit=2&order=" + order
		for path != "" {
			m := e.get(path).json(t)
			for _, x := range m["data"].([]any) {
				ids = append(ids, x.(map[string]any)["id"].(string))
			}
			path = ""
			if nc, ok := m["next_cursor"].(string); ok {
				path = "/api/v1/chats/any;-;+15550000001/messages?limit=2&order=" + order + "&cursor=" + nc
			}
		}
		return ids
	}
	desc := collect("desc")
	asc := collect("asc")
	want := []string{e.ids["hello"], e.ids["reply"], e.ids["love"], e.ids["edited"], e.ids["unsent"]}
	if strings.Join(asc, ",") != strings.Join(want, ",") {
		t.Fatalf("asc = %v", asc)
	}
	for i := range desc {
		if desc[i] != want[len(want)-1-i] {
			t.Fatalf("desc = %v", desc)
		}
	}
	// A desc cursor cannot continue an asc listing.
	m := e.get("/api/v1/chats/any;-;+15550000001/messages?limit=1").json(t)
	wantStatus(t, e.get("/api/v1/chats/any;-;+15550000001/messages?order=asc&cursor="+m["next_cursor"].(string)), 400, "bad_request")
	// Nor another chat's listing.
	wantStatus(t, e.get("/api/v1/chats/any;+;chat0001/messages?cursor="+m["next_cursor"].(string)), 400, "bad_request")
	wantStatus(t, e.get("/api/v1/chats/any;-;+15550000001/messages?order=up"), 400, "bad_request")
}

func TestSync(t *testing.T) {
	e := newEnv(t)
	var all []map[string]any
	since := 0.0
	for {
		m := e.get("/api/v1/messages?limit=4&since_id=" + jsonNum(since)).json(t)
		for _, x := range m["data"].([]any) {
			all = append(all, x.(map[string]any))
		}
		next := m["next_since_id"].(float64)
		if !m["has_more"].(bool) {
			if len(m["data"].([]any)) > 0 && next <= since {
				t.Fatal("next_since_id did not advance")
			}
			break
		}
		since = next
	}
	if len(all) != 11 {
		t.Fatalf("synced %d messages, want 11", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i]["rowid"].(float64) <= all[i-1]["rowid"].(float64) {
			t.Fatal("not ascending")
		}
	}
	// Empty page echoes since_id.
	last := all[len(all)-1]["rowid"].(float64)
	m := e.get("/api/v1/messages?since_id=" + jsonNum(last)).json(t)
	if len(m["data"].([]any)) != 0 || m["next_since_id"].(float64) != last || m["has_more"] != false {
		t.Fatalf("empty page = %v", m)
	}
	dm := e.get("/api/v1/messages?chat_id=any%3B-%3B%2B15550000001").json(t)["data"].([]any)
	if len(dm) != 5 {
		t.Fatalf("chat filter returned %d", len(dm))
	}
	// A raw ";" invalidates the query string; it must not be silently ignored.
	wantStatus(t, e.get("/api/v1/messages?chat_id=any;-;%2B15550000001"), 400, "bad_request")
}

func jsonNum(f float64) string { b, _ := json.Marshal(int64(f)); return string(b) }

func TestMessageShapes(t *testing.T) {
	e := newEnv(t)
	msg := func(key string) map[string]any {
		t.Helper()
		r := e.get("/api/v1/messages/" + e.ids[key])
		wantStatus(t, r, 200, "")
		return r.json(t)
	}

	hello := msg("hello")
	if hello["type"] != "message" || hello["text"] != "hello" || hello["from_me"] != false {
		t.Fatalf("hello = %v", hello)
	}
	if s := hello["sender"].(map[string]any); s["address"] != "+15550000001" || s["name"] != "Alice" {
		t.Fatalf("sender = %v", s)
	}

	reply := msg("reply")
	if reply["sender"] != nil || reply["read_at"] != "2026-03-01T10:03:00Z" || reply["is_delivered"] != true {
		t.Fatalf("reply = %v", reply)
	}

	love := msg("love")
	r := love["reaction"].(map[string]any)
	if love["type"] != "reaction" || r["kind"] != "love" || r["target_id"] != e.ids["reply"] || r["emoji"] != nil {
		t.Fatalf("love = %v", love)
	}
	emoji := msg("emoji")["reaction"].(map[string]any)
	if emoji["kind"] != "emoji" || emoji["emoji"] != "🔥" || emoji["target_part"] != 1.0 {
		t.Fatalf("emoji = %v", emoji)
	}

	edited := msg("edited")
	edits := edited["edits"].([]any)
	if edited["edited_at"] == nil || len(edits) != 1 || edits[0].(map[string]any)["text"] != "helo" || edited["unsent"] != false {
		t.Fatalf("edited = %v", edited)
	}
	unsent := msg("unsent")
	if unsent["unsent"] != true || unsent["edited_at"] != nil || unsent["text"] != "" {
		t.Fatalf("unsent = %v", unsent)
	}

	photo := msg("photo")
	parts := photo["parts"].([]any)
	atts := photo["attachments"].([]any)
	if photo["text"] != "look" || len(parts) != 2 || parts[1].(map[string]any)["attachment_id"] != "ATT-PHOTO" || len(atts) != 1 {
		t.Fatalf("photo = %v", photo)
	}
	if a := atts[0].(map[string]any); a["available"] != true || a["message_id"] != e.ids["photo"] || a["filename"] != "IMG_1.jpg" {
		t.Fatalf("attachment = %v", a)
	}

	// Group event and hidden attachments, via the group's history.
	page := e.get("/api/v1/chats/any;+;chat0001/messages?order=asc").json(t)["data"].([]any)
	var sawEvent bool
	for _, x := range page {
		m := x.(map[string]any)
		for _, a := range m["attachments"].([]any) {
			if a.(map[string]any)["id"] == "ATT-HIDDEN" {
				t.Fatal("hidden attachment exposed")
			}
		}
		if m["type"] == "group_event" {
			ge := m["group_event"].(map[string]any)
			if ge["kind"] != "participant_added" || ge["participant"].(map[string]any)["address"] != "+15550000001" {
				t.Fatalf("group event = %v", ge)
			}
			sawEvent = true
		}
		if m["app"] == "com.apple.messages.URLBalloonProvider" && len(m["parts"].([]any)) != 1 {
			t.Fatalf("link message parts = %v", m["parts"])
		}
	}
	if !sawEvent {
		t.Fatal("no group event")
	}
}

func TestAttachmentContent(t *testing.T) {
	e := newEnv(t)
	meta := e.get("/api/v1/attachments/ATT-PHOTO")
	wantStatus(t, meta, 200, "")
	if meta.json(t)["mime_type"] != "image/jpeg" {
		t.Fatalf("meta = %s", meta.body)
	}

	full := e.get("/api/v1/attachments/ATT-PHOTO/content")
	if full.status != 200 || len(full.body) != 1024 || full.header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("content: %d %d %s", full.status, len(full.body), full.header.Get("Content-Type"))
	}
	if cd := full.header.Get("Content-Disposition"); !strings.Contains(cd, "IMG_1.jpg") {
		t.Fatalf("disposition = %s", cd)
	}
	part := e.get("/api/v1/attachments/ATT-PHOTO/content", func(r *http.Request) { r.Header.Set("Range", "bytes=0-7") })
	if part.status != 206 || string(part.body) != "JPEGDATA" {
		t.Fatalf("range: %d %q", part.status, part.body)
	}

	wantStatus(t, e.get("/api/v1/attachments/ATT-MISSING/content"), 404, "attachment_unavailable")
	if e.get("/api/v1/attachments/ATT-MISSING").json(t)["available"] != false {
		t.Fatal("missing attachment reported available")
	}
	wantStatus(t, e.get("/api/v1/attachments/ATT-HIDDEN"), 404, "not_found")
	// /etc/hosts exists but is outside the allowed roots.
	wantStatus(t, e.get("/api/v1/attachments/ATT-OUTSIDE/content"), 404, "attachment_unavailable")

	// A symlink inside the root pointing outside is refused.
	link := filepath.Join(e.root, "IMG_1.jpg")
	os.Remove(link)
	os.Symlink("/etc/hosts", link)
	wantStatus(t, e.get("/api/v1/attachments/ATT-PHOTO/content"), 404, "attachment_unavailable")
}

func TestHandles(t *testing.T) {
	e := newEnv(t)
	m := e.get("/api/v1/handles?q=0002").json(t)
	data := m["data"].([]any)
	if len(data) != 1 || data[0].(map[string]any)["address"] != "+15550000002" {
		t.Fatalf("handles = %v", m)
	}
	page := e.get("/api/v1/handles?limit=1").json(t)
	next := e.get("/api/v1/handles?limit=1&cursor=" + page["next_cursor"].(string)).json(t)
	if next["data"].([]any)[0].(map[string]any)["address"] != "+15550000002" || next["next_cursor"] != nil {
		t.Fatalf("handle paging = %v", next)
	}
}
