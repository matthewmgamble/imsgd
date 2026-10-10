package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/matthewmgamble/imsgd/internal/events"
)

const (
	wsPingInterval = 30 * time.Second
	wsWriteTimeout = 10 * time.Second
)

// helloFrame is the first frame on every connection.
type helloFrame struct {
	Type string    `json:"type"` // "hello"
	Data helloData `json:"data"`
}

type helloData struct {
	ServerVersion string `json:"server_version"`
	// Resumed is false when last_event_id was given but events since then
	// are no longer available (server restarted or buffer overflowed). The
	// client should resynchronise via REST (GET /api/v1/messages?since_id=).
	Resumed     bool    `json:"resumed"`
	LastEventID *string `json:"last_event_id"`
	Replayed    int     `json:"replayed"`
}

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) error {
	if s.bus == nil {
		return &apiError{http.StatusServiceUnavailable, codeInternal, "events are not enabled"}
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return badRequest("this endpoint requires a WebSocket upgrade")
	}
	q := r.URL.Query()
	var f events.Filter
	if v := q.Get("types"); v != "" {
		f.Types = strings.Split(v, ",")
		if err := events.ValidateTypes(f.Types); err != nil {
			return badRequest("types: %v", err)
		}
	}
	f.ChatIDs = q["chat_id"]
	f.Owner = keyNameOf(r)
	if !s.streams.acquire(f.Owner) {
		tooManyRequests(w, "too many open event streams for this API key", 0)
		return nil
	}
	defer s.streams.release(f.Owner)

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Authentication is by API key, not cookies, so cross-origin
		// browser clients are safe to accept.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return nil // Accept already wrote an error response
	}
	defer conn.CloseNow()

	sub, backlog, resumed := s.bus.Subscribe(f, q.Get("last_event_id"), 512)
	defer sub.Close()

	ctx := conn.CloseRead(r.Context()) // we never read; this handles pings/close
	last := s.bus.LastID()
	hello := helloFrame{Type: "hello", Data: helloData{
		ServerVersion: s.version, Resumed: resumed, LastEventID: strPtrOrNil(last), Replayed: len(backlog),
	}}
	if err := writeFrame(ctx, conn, hello); err != nil {
		return nil
	}
	for _, ev := range backlog {
		if err := writeFrame(ctx, conn, ev); err != nil {
			return nil
		}
	}

	ping := time.NewTicker(wsPingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.shutdown:
			conn.Close(websocket.StatusGoingAway, "server shutting down")
			return nil
		case <-sub.Done():
			slog.Info("closing slow WebSocket client", "remote", r.RemoteAddr)
			conn.Close(websocket.StatusTryAgainLater, "client too slow; reconnect with last_event_id")
			return nil
		case ev := <-sub.C():
			if err := writeFrame(ctx, conn, ev); err != nil {
				return nil
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return nil
			}
		}
	}
}

func writeFrame(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	err = conn.Write(wctx, websocket.MessageText, b)
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Debug("websocket write failed", "err", err)
	}
	return err
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
