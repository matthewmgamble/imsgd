package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matthewmgamble/imsgd/internal/imessage/fixture"

	"github.com/getkin/kin-openapi/openapi3filter"
)

// multipartBody builds a send request with attachments.
func multipartBody(t *testing.T, payload map[string]any, files map[string][]byte) (string, []byte) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	p, _ := json.Marshal(payload)
	mw.WriteField("payload", string(p))
	for name, data := range files {
		w, _ := mw.CreateFormFile("attachments", name)
		w.Write(data)
	}
	mw.Close()
	return mw.FormDataContentType(), buf.Bytes()
}

func (e *env) postMultipart(ct string, body []byte) resp {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", ct)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	// Validate the response against the spec.
	req2, _ := http.NewRequest("POST", e.ts.URL+"/api/v1/messages", bytes.NewReader(body))
	req2.Header.Set("Content-Type", ct)
	route, params, _ := e.router.FindRoute(req2)
	in := &openapi3filter.RequestValidationInput{Request: req2, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, ExcludeRequestBody: true}}
	if err := openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
		RequestValidationInput: in, Status: res.StatusCode, Header: res.Header, Body: io.NopCloser(bytes.NewReader(b)),
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}); err != nil {
		e.t.Errorf("multipart response does not match spec: %v\n%s", err, b)
	}
	return resp{res.StatusCode, res.Header, b}
}

func TestSendText(t *testing.T) {
	e := newEnv(t)
	r := e.doBody("POST", "/api/v1/messages", map[string]any{"chat_id": "any;-;+15550000001", "text": "on my way", "client_ref": "r1"})
	wantStatus(t, r, 201, "")
	m := r.json(t)
	snd := m["send"].(map[string]any)
	msgs := m["messages"].([]any)
	if snd["status"] != "sent" || len(msgs) != 1 || msgs[0].(map[string]any)["text"] != "on my way" {
		t.Fatalf("send = %s", r.body)
	}
	item := snd["items"].([]any)[0].(map[string]any)
	if item["message_id"] != msgs[0].(map[string]any)["id"] {
		t.Fatal("item not linked to message")
	}

	// Idempotent replay: same send, no second call to Messages.
	again := e.doBody("POST", "/api/v1/messages", map[string]any{"chat_id": "any;-;+15550000001", "text": "on my way", "client_ref": "r1"})
	wantStatus(t, again, 200, "")
	if again.header.Get("Idempotent-Replay") != "true" || again.json(t)["send"].(map[string]any)["id"] != snd["id"] {
		t.Fatalf("replay = %s", again.body)
	}
	if e.runner.CallCount() != 1 {
		t.Fatalf("sent %d times", e.runner.CallCount())
	}

	// Lookup by ID; private to the creating key.
	id := snd["id"].(string)
	wantStatus(t, e.get("/api/v1/sends/"+id), 200, "")
	wantStatus(t, e.get("/api/v1/sends/"+id, otherKeyAuth), 404, "not_found")
	wantStatus(t, e.get("/api/v1/sends/nope"), 404, "not_found")
}

func TestSendToAddressNoWait(t *testing.T) {
	e := newEnv(t)
	r := e.doBody("POST", "/api/v1/messages", map[string]any{"to": "+1 (555) 123-4567", "text": "hi", "wait": false})
	wantStatus(t, r, 202, "")
	id := r.json(t)["send"].(map[string]any)["id"].(string)
	// Eventually sent.
	for i := 0; i < 200; i++ {
		s := e.get("/api/v1/sends/" + id).json(t)["send"].(map[string]any)
		if s["status"] == "sent" {
			if s["chat_id"] != "any;-;+15551234567" {
				t.Fatalf("chat_id = %v", s["chat_id"])
			}
			return
		}
		sleep()
	}
	t.Fatal("never sent")
}

func TestSendFailures(t *testing.T) {
	e := newEnv(t)
	e.runner.FailNext(errors.New("Messages got an error: Can’t get chat"))
	r := e.doBody("POST", "/api/v1/messages", map[string]any{"chat_id": "any;-;+15550000001", "text": "x"})
	wantStatus(t, r, 201, "")
	if s := r.json(t)["send"].(map[string]any); s["status"] != "failed" || !strings.Contains(s["error"].(string), "Can’t get chat") {
		t.Fatalf("send = %v", s)
	}
	// The message never appears in chat.db: the text timeout (300ms in
	// tests) fires within the wait window, so the send settles as failed.
	e.runner.DropNext()
	r = e.doBody("POST", "/api/v1/messages", map[string]any{"chat_id": "any;-;+15550000001", "text": "lost"})
	if s := r.json(t)["send"].(map[string]any); r.status != 201 || s["status"] != "failed" {
		t.Fatalf("dropped send = %d %s", r.status, r.body)
	}
}

func TestSendValidation(t *testing.T) {
	e := newEnv(t)
	cases := []any{
		map[string]any{"text": "no target"},
		map[string]any{"chat_id": "any;-;+15550000001", "to": "+15550000001", "text": "both"},
		map[string]any{"chat_id": "any;-;+15550000001"},
		map[string]any{"chat_id": "any;-;+15550000001", "text": "   "},
		map[string]any{"to": "not an address", "text": "x"},
		map[string]any{"to": "+15550000001", "service": "Carrier Pigeon", "text": "x"},
		map[string]any{"chat_id": "any;-;+15550000001", "service": "SMS", "text": "x"},
		map[string]any{"chat_id": "any;-;+15550000001", "text": "x", "unknown": 1},
		map[string]any{"chat_id": "any;-;+15550000001", "text": strings.Repeat("x", 20001)},
		"{broken",
	}
	for i, c := range cases {
		if r := e.doBody("POST", "/api/v1/messages", c); r.status != 400 {
			t.Errorf("case %d: status %d %s", i, r.status, r.body)
		}
	}
	wantStatus(t, e.doBody("POST", "/api/v1/messages", map[string]any{"chat_id": "any;-;nope", "text": "x"}), 404, "not_found")
	if e.runner.CallCount() != 0 {
		t.Fatal("invalid requests reached Messages")
	}
}

func TestSendAttachments(t *testing.T) {
	e := newEnv(t)
	ct, body := multipartBody(t, map[string]any{"chat_id": "any;-;+15550000001", "text": "photos"},
		map[string][]byte{"../../evil name.jpg": []byte("JPEG1")})
	r := e.postMultipart(ct, body)
	wantStatus(t, r, 201, "")
	m := r.json(t)
	items := m["send"].(map[string]any)["items"].([]any)
	if len(items) != 2 || items[1].(map[string]any)["filename"] != "evil name.jpg" || len(m["messages"].([]any)) != 2 {
		t.Fatalf("send = %s", r.body)
	}
	// The file was staged under the staging dir with its sanitized name.
	var staged string
	for _, c := range e.runner.Calls() {
		if strings.HasPrefix(c, "file-chat") {
			staged = c[strings.Index(c, e.root):]
		}
	}
	if !strings.HasPrefix(staged, e.root+"/staging/") || !strings.HasSuffix(staged, "/evil name.jpg") {
		t.Fatalf("staged at %q", staged)
	}
	if b, err := os.ReadFile(staged); err != nil || string(b) != "JPEG1" {
		t.Fatalf("staged content: %q %v", b, err)
	}

	// Too large (env limit is 1 MiB).
	ct, body = multipartBody(t, map[string]any{"chat_id": "any;-;+15550000001"}, map[string][]byte{"big.bin": bytes.Repeat([]byte("x"), 2<<20)})
	if r := e.postMultipart(ct, body); r.status != 413 {
		t.Fatalf("oversize: %d %s", r.status, r.body)
	}
	// Too many files.
	files := map[string][]byte{}
	for i := 0; i < 11; i++ {
		files[string(rune('a'+i))+".txt"] = []byte("x")
	}
	ct, body = multipartBody(t, map[string]any{"chat_id": "any;-;+15550000001"}, files)
	if r := e.postMultipart(ct, body); r.status != 400 {
		t.Fatalf("too many: %d", r.status)
	}
}

func sleep() { <-time.After(10 * time.Millisecond) }

func TestSendRateLimit(t *testing.T) {
	e := newEnv(t)
	e.srv.sendLimit = newRateLimiter(1, 2) // 2 burst, then 1/min
	for i := 0; i < 2; i++ {
		r := e.doBody("POST", "/api/v1/messages", map[string]any{"chat_id": "any;-;+15550000001", "text": "x", "wait": false})
		if r.status != 202 && r.status != 201 {
			t.Fatalf("send %d: %d", i, r.status)
		}
	}
	r := e.doBody("POST", "/api/v1/messages", map[string]any{"chat_id": "any;-;+15550000001", "text": "x"})
	wantStatus(t, r, 429, "rate_limited")
	if r.header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}
	// Other keys have their own bucket.
	if r := e.doBody("POST", "/api/v1/messages", map[string]any{"chat_id": "any;-;+15550000001", "text": "y", "wait": false}, otherKeyAuth); r.status == 429 {
		t.Fatal("rate limit shared across keys")
	}
}

func TestActiveContentNotInline(t *testing.T) {
	e := newEnv(t)
	html := filepath.Join(e.root, "evil", "x.html")
	os.MkdirAll(filepath.Dir(html), 0o700)
	os.WriteFile(html, []byte("<script>alert(1)</script>"), 0o600)
	id, _ := e.f.Message(fixture.Msg{Chat: 1, Handle: 1, Text: "￼"})
	e.f.Attachment(id, fixture.Att{GUID: "ATT-HTML", Filename: html, MimeType: "text/html", TransferName: "x.html"})
	r := e.get("/api/v1/attachments/ATT-HTML/content")
	if r.status != 200 || r.header.Get("Content-Type") != "application/octet-stream" ||
		!strings.HasPrefix(r.header.Get("Content-Disposition"), "attachment") ||
		!strings.Contains(r.header.Get("Content-Security-Policy"), "sandbox") ||
		r.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers = %v", r.header)
	}
	img := e.get("/api/v1/attachments/ATT-PNG/content")
	if !strings.HasPrefix(img.header.Get("Content-Disposition"), "inline") || img.header.Get("Content-Type") != "image/png" {
		t.Fatalf("png headers = %v", img.header)
	}
}
