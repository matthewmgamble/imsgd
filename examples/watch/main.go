// Command watch is an example imsgd client: it streams events over
// WebSocket, reconnecting and resuming from the last event it saw.
//
//	IMSGD_KEY=... go run ./examples/watch -url http://mac.local:8642 -types message.*
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type event struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

type messageData struct {
	Message struct {
		ID     string  `json:"id"`
		ChatID *string `json:"chat_id"`
		Type   string  `json:"type"`
		FromMe bool    `json:"from_me"`
		Text   string  `json:"text"`
		Sender *struct {
			Address string `json:"address"`
		} `json:"sender"`
		IsDelivered bool `json:"is_delivered"`
		Error       int  `json:"error"`
	} `json:"message"`
	Changes []string `json:"changes"`
}

type chats []string

func (c *chats) String() string     { return strings.Join(*c, ",") }
func (c *chats) Set(v string) error { *c = append(*c, v); return nil }

func main() {
	base := flag.String("url", "http://localhost:8642", "imsgd base URL")
	types := flag.String("types", "", "comma-separated event types (default all)")
	raw := flag.Bool("raw", false, "print raw JSON frames")
	var chatIDs chats
	flag.Var(&chatIDs, "chat", "only events for this chat ID (repeatable)")
	flag.Parse()
	key := os.Getenv("IMSGD_KEY")
	if key == "" {
		log.Fatal("set IMSGD_KEY")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	lastID := ""
	backoff := time.Second
	for ctx.Err() == nil {
		q := url.Values{}
		if *types != "" {
			q.Set("types", *types)
		}
		for _, c := range chatIDs {
			q.Add("chat_id", c)
		}
		if lastID != "" {
			q.Set("last_event_id", lastID)
		}
		wsURL := "ws" + strings.TrimPrefix(*base, "http") + "/api/v1/events?" + q.Encode()
		conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": {"Bearer " + key}},
		})
		if err != nil {
			log.Printf("connect failed: %v (retrying in %s)", err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		conn.SetReadLimit(16 << 20)
		for {
			_, b, err := conn.Read(ctx)
			if err != nil {
				log.Printf("disconnected: %v", err)
				break
			}
			if *raw {
				fmt.Println(string(b))
			}
			var probe struct {
				Type string `json:"type"`
				Data struct {
					Resumed  bool `json:"resumed"`
					Replayed int  `json:"replayed"`
				} `json:"data"`
			}
			json.Unmarshal(b, &probe)
			if probe.Type == "hello" {
				log.Printf("connected (resumed=%v, replayed=%d)", probe.Data.Resumed, probe.Data.Replayed)
				if !probe.Data.Resumed {
					log.Printf("events were missed; a real client would now resync via GET /api/v1/messages?since_id=")
				}
				continue
			}
			var ev event
			json.Unmarshal(b, &ev)
			lastID = ev.ID
			if !*raw {
				printEvent(ev)
			}
		}
		conn.CloseNow()
	}
}

func printEvent(ev event) {
	ts := ev.CreatedAt.Local().Format("15:04:05")
	switch {
	case strings.HasPrefix(ev.Type, "message."):
		var d messageData
		json.Unmarshal(ev.Data, &d)
		m := d.Message
		who := "me"
		if m.Sender != nil {
			who = m.Sender.Address
		}
		extra := ""
		if len(d.Changes) > 0 {
			extra = " changes=" + strings.Join(d.Changes, ",")
		}
		fmt.Printf("%s %-16s %s [%s] %s: %q delivered=%v error=%d%s\n",
			ts, ev.Type, m.ID[:8], m.Type, who, m.Text, m.IsDelivered, m.Error, extra)
	default:
		fmt.Printf("%s %-16s %s\n", ts, ev.Type, ev.Data)
	}
}
