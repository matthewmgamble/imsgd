package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/matthewmgamble/imsgd/internal/config"
	"github.com/matthewmgamble/imsgd/internal/events"
	"github.com/matthewmgamble/imsgd/internal/imessage/db"
	"github.com/matthewmgamble/imsgd/internal/imessage/fixture"
	"github.com/matthewmgamble/imsgd/internal/present"
	"github.com/matthewmgamble/imsgd/internal/tracker"
)

// TestLoad exercises sync throughput, WebSocket fan-out and concurrent reads.
// Run with: IMSGD_LOAD=1 go test ./internal/api -run TestLoad -v
func TestLoad(t *testing.T) {
	if os.Getenv("IMSGD_LOAD") == "" {
		t.Skip("set IMSGD_LOAD=1 to run the load test")
	}
	const (
		nChats    = 200
		nMessages = 100_000
		nClients  = 50
		nBurst    = 200
	)
	f := fixture.New(t)
	f.Fast()
	var chats []int64
	var guids []string
	for i := 0; i < nChats; i++ {
		h := f.Handle(fmt.Sprintf("+1555%07d", i), "iMessage")
		guid := fmt.Sprintf("any;-;+1555%07d", i)
		chats = append(chats, f.Chat(guid, fixture.StyleDirect, "", h))
		guids = append(guids, guid)
	}
	start := time.Now()
	base := time.Now().Add(-90 * 24 * time.Hour)
	f.DB.Exec("BEGIN")
	for i := 0; i < nMessages; i++ {
		f.Message(fixture.Msg{Chat: chats[i%nChats], Handle: int64(i%nChats + 1), FromMe: i%3 == 0,
			Text: fmt.Sprintf("message %d with some typical length of text in it", i), Date: base.Add(time.Duration(i) * time.Minute)})
	}
	f.DB.Exec("COMMIT")
	t.Logf("fixture: %d messages in %d chats built in %s", nMessages, nChats, time.Since(start).Round(time.Millisecond))

	d, err := db.Open(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	bus := events.NewBus(events.DefaultBufferSize)
	pres := &present.Presenter{DB: d}
	trk := tracker.New(tracker.Options{DB: d, Bus: bus, Presenter: pres})
	if err := trk.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
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
	srv := New(Options{DB: d, Bus: bus, APIKeys: []config.APIKey{{Name: "load", Key: testKey}}})
	ts := httptest.NewServer(srv)
	defer ts.Close()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	get := func(path string) (*http.Response, error) {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		return client.Do(req)
	}

	// 1. Full sync over HTTP.
	start = time.Now()
	since, total := int64(0), 0
	for {
		res, err := get(fmt.Sprintf("/api/v1/messages?limit=1000&since_id=%d", since))
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Data        []json.RawMessage `json:"data"`
			NextSinceID int64             `json:"next_since_id"`
			HasMore     bool              `json:"has_more"`
		}
		json.NewDecoder(res.Body).Decode(&page)
		res.Body.Close()
		total += len(page.Data)
		since = page.NextSinceID
		if !page.HasMore {
			break
		}
	}
	dur := time.Since(start)
	if total != nMessages {
		t.Fatalf("synced %d of %d", total, nMessages)
	}
	t.Logf("full sync: %d messages in %s (%.0f msg/s)", total, dur.Round(time.Millisecond), float64(total)/dur.Seconds())

	// 2. WebSocket fan-out: nClients each receive a burst of nBurst events.
	var conns []*websocket.Conn
	for i := 0; i < nClients; i++ {
		c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/events",
			&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + testKey}}})
		if err != nil {
			t.Fatal(err)
		}
		c.SetReadLimit(1 << 20)
		defer c.CloseNow()
		conns = append(conns, c)
	}
	var received atomic.Int64
	var wg sync.WaitGroup
	done := make(chan struct{})
	for _, c := range conns {
		wg.Add(1)
		go func(c *websocket.Conn) {
			defer wg.Done()
			c.Read(ctx) // hello
			for n := 0; n < nBurst; n++ {
				rctx, rcancel := context.WithTimeout(ctx, 20*time.Second)
				_, _, err := c.Read(rctx)
				rcancel()
				if err != nil {
					return
				}
				received.Add(1)
			}
		}(c)
	}
	go func() { wg.Wait(); close(done) }()
	time.Sleep(200 * time.Millisecond)
	start = time.Now()
	f.DB.Exec("BEGIN")
	for i := 0; i < nBurst; i++ {
		f.Message(fixture.Msg{Chat: chats[i%nChats], Handle: int64(i%nChats + 1), Text: fmt.Sprintf("burst %d", i), Date: time.Now()})
	}
	f.DB.Exec("COMMIT")
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
	want := int64(nClients * nBurst)
	t.Logf("fan-out: %d/%d events delivered to %d clients in %s", received.Load(), want, nClients, time.Since(start).Round(time.Millisecond))
	if received.Load() != want {
		t.Fatalf("lost events: %d/%d", received.Load(), want)
	}

	// 3. Concurrent reads for 5 s.
	var lat []time.Duration
	var mu sync.Mutex
	var errs atomic.Int64
	deadline := time.Now().Add(5 * time.Second)
	var rw sync.WaitGroup
	for w := 0; w < 20; w++ {
		rw.Add(1)
		go func(w int) {
			defer rw.Done()
			for i := 0; time.Now().Before(deadline); i++ {
				path := "/api/v1/chats?limit=50&include=participants,last_message"
				if i%2 == 1 {
					path = "/api/v1/chats/" + guids[(w*31+i)%nChats] + "/messages?limit=100"
				}
				t0 := time.Now()
				res, err := get(path)
				if err != nil || res.StatusCode != 200 {
					errs.Add(1)
					if res != nil {
						res.Body.Close()
					}
					continue
				}
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				mu.Lock()
				lat = append(lat, time.Since(t0))
				mu.Unlock()
			}
		}(w)
	}
	rw.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) time.Duration { return lat[int(float64(len(lat)-1)*p)].Round(time.Microsecond) }
	t.Logf("concurrent reads (20 workers, 5s): %d requests (%.0f req/s), errors %d, p50 %s p95 %s p99 %s",
		len(lat), float64(len(lat))/5, errs.Load(), pct(0.5), pct(0.95), pct(0.99))
	if errs.Load() > 0 {
		t.Fatalf("%d request errors", errs.Load())
	}
}
