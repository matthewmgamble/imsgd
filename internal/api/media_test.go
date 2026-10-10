package api

import (
	"net/http"
	"os"
	"testing"

	"github.com/matthewmgamble/imsgd/internal/media"
)

func TestAttachmentMetadataDimensionsAndLive(t *testing.T) {
	e := newEnv(t)
	m := e.get("/api/v1/attachments/ATT-PNG").json(t)
	if m["width"] != 640.0 || m["height"] != 480.0 || m["live_photo"] != true {
		t.Fatalf("png = %v", m)
	}
	// Undecodable "image" (fake bytes): dimensions unknown, no live photo.
	j := e.get("/api/v1/attachments/ATT-PHOTO").json(t)
	if j["width"] != nil || j["live_photo"] != false {
		t.Fatalf("photo = %v", j)
	}
}

func TestAttachmentFormats(t *testing.T) {
	e := newEnv(t)
	base := "/api/v1/attachments/ATT-PNG/content"

	r := e.get(base + "?format=jpeg")
	if r.status != 200 || r.header.Get("Content-Type") != "image/jpeg" || len(r.body) < 100 {
		t.Fatalf("jpeg: %d %s %d bytes", r.status, r.header.Get("Content-Type"), len(r.body))
	}
	path := e.writeTemp(t, r.body)
	if w, h, err := media.Dimensions(path); err != nil || w != 640 || h != 480 {
		t.Fatalf("converted dims %dx%d %v", w, h, err)
	}
	thumb := e.get(base + "?format=jpeg&max_size=64")
	if w, h, _ := media.Dimensions(e.writeTemp(t, thumb.body)); w != 64 || h != 48 {
		t.Fatalf("thumb %dx%d", w, h)
	}
	if cd := thumb.header.Get("Content-Disposition"); cd != `inline; filename=pic.jpg` {
		t.Fatalf("disposition = %q", cd)
	}

	live := e.get(base + "?format=live")
	if live.status != 200 || string(live.body) != "MOVDATA" || live.header.Get("Content-Type") != "video/quicktime" {
		t.Fatalf("live: %d %q", live.status, live.body)
	}
	ranged := e.get(base+"?format=live", func(r *http.Request) { r.Header.Set("Range", "bytes=0-2") })
	if ranged.status != 206 || string(ranged.body) != "MOV" {
		t.Fatalf("live range: %d %q", ranged.status, ranged.body)
	}

	wantStatus(t, e.get(base+"?format=m4a"), 400, "bad_request")   // image → audio
	wantStatus(t, e.get(base+"?format=gif"), 400, "bad_request")   // unknown format
	wantStatus(t, e.get(base+"?max_size=100"), 400, "bad_request") // needs jpeg
	wantStatus(t, e.get(base+"?format=jpeg&max_size=5"), 400, "bad_request")
	wantStatus(t, e.get("/api/v1/attachments/ATT-PHOTO/content?format=live"), 404, "attachment_unavailable")
	// Bytes that only claim to be a JPEG fail conversion cleanly.
	wantStatus(t, e.get("/api/v1/attachments/ATT-PHOTO/content?format=jpeg"), 422, "conversion_failed")
}

func TestContactNamesInMessages(t *testing.T) {
	e := newEnv(t)
	// The env's resolver names +15550000001 "Alice".
	m := e.get("/api/v1/chats/any%3B%2B%3Bchat0001").json(t)
	var named int
	for _, p := range m["participants"].([]any) {
		if p.(map[string]any)["name"] == "Alice" {
			named++
		}
	}
	if named != 1 {
		t.Fatalf("participants = %v", m["participants"])
	}
}

func (e *env) writeTemp(t *testing.T, b []byte) string {
	t.Helper()
	p := t.TempDir() + "/out"
	if err := writeFileHelper(p, b); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFileHelper(p string, b []byte) error { return os.WriteFile(p, b, 0o600) }
