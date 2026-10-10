// Package api implements the imsgd HTTP API described by api/openapi.yaml.
package api

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"github.com/matthewmgamble/imsgd/internal/config"
	"github.com/matthewmgamble/imsgd/internal/events"
	"github.com/matthewmgamble/imsgd/internal/imessage/db"
	"github.com/matthewmgamble/imsgd/internal/media"
	"github.com/matthewmgamble/imsgd/internal/present"
	"github.com/matthewmgamble/imsgd/internal/send"
	"github.com/matthewmgamble/imsgd/internal/store"
)

// Options configures a Server.
type Options struct {
	DB           *db.DB
	APIKeys      []config.APIKey
	Version      string
	MacOSVersion string
	// Names resolves contact names; nil disables resolution.
	Names present.NameResolver
	// AttachmentRoots are directories attachment content may be served from.
	// Defaults to ~/Library/Messages/Attachments and StickerCache (never the
	// database files beside them).
	AttachmentRoots []string
	// Bus enables GET /api/v1/events; Store and Webhooks enable the webhook API.
	Bus      *events.Bus
	Store    *store.Store
	Webhooks WebhookDispatcher
	// Sender enables POST /api/v1/messages and GET /api/v1/sends/{id}.
	Sender *send.Sender
	// UploadMaxBytes limits a send request body (default 100 MiB).
	UploadMaxBytes int64
	// Converter enables format=jpeg/m4a on attachment content.
	Converter *media.Converter
	// Dims enables real image dimensions in attachment objects.
	Dims *media.DimensionCache
	// CORSOrigins lists browser origins allowed to call the API ("*" = any).
	CORSOrigins []string
	// SendsPerMinute limits POST /api/v1/messages per API key (default 60,
	// burst 20). StreamsPerKey caps concurrent WebSockets per key (default 20).
	SendsPerMinute int
	StreamsPerKey  int
}

type Server struct {
	db        *db.DB
	auth      *authenticator
	version   string
	macOS     string
	startedAt time.Time
	present   *present.Presenter
	roots     []string
	handler   http.Handler
	bus       *events.Bus
	store     *store.Store
	hooks     WebhookDispatcher
	shutdown  chan struct{}
	closeOnce sync.Once
	sender    *send.Sender
	uploadMax int64
	converter *media.Converter
	sendLimit *rateLimiter
	streams   *connLimiter
}

// route is one API endpoint. Pattern uses net/http syntax, which matches the
// OpenAPI path template syntax, so the contract test can compare the two.
type route struct {
	Method  string
	Pattern string
	Public  bool // no API key required
	Stream  bool // no request timeout (file downloads, WebSocket)
	// TokenQuery also accepts the API key as ?token= (WebSocket only:
	// browsers cannot set headers on WebSocket connections).
	TokenQuery bool
	Handle     func(s *Server, w http.ResponseWriter, r *http.Request) error
}

var routes = []route{
	{Method: "GET", Pattern: "/health", Public: true, Handle: (*Server).getHealth},
	{Method: "GET", Pattern: "/api/v1/server", Handle: (*Server).getServer},
	{Method: "GET", Pattern: "/api/v1/openapi.yaml", Handle: (*Server).getOpenAPI},
	{Method: "GET", Pattern: "/api/v1/chats", Handle: (*Server).listChats},
	{Method: "GET", Pattern: "/api/v1/chats/{chat_id}", Handle: (*Server).getChat},
	{Method: "GET", Pattern: "/api/v1/chats/{chat_id}/messages", Handle: (*Server).listChatMessages},
	{Method: "GET", Pattern: "/api/v1/messages", Handle: (*Server).syncMessages},
	{Method: "POST", Pattern: "/api/v1/messages", Stream: true, Handle: (*Server).createSend},
	{Method: "GET", Pattern: "/api/v1/sends/{send_id}", Handle: (*Server).getSend},
	{Method: "GET", Pattern: "/api/v1/messages/{message_id}", Handle: (*Server).getMessage},
	{Method: "GET", Pattern: "/api/v1/handles", Handle: (*Server).listHandles},
	{Method: "GET", Pattern: "/api/v1/attachments/{attachment_id}", Handle: (*Server).getAttachment},
	{Method: "GET", Pattern: "/api/v1/attachments/{attachment_id}/content", Stream: true, Handle: (*Server).getAttachmentContent},
	{Method: "GET", Pattern: "/api/v1/events", Stream: true, TokenQuery: true, Handle: (*Server).streamEvents},
	{Method: "GET", Pattern: "/api/v1/webhooks", Handle: (*Server).listWebhooks},
	{Method: "POST", Pattern: "/api/v1/webhooks", Handle: (*Server).createWebhook},
	{Method: "GET", Pattern: "/api/v1/webhooks/{webhook_id}", Handle: (*Server).getWebhook},
	{Method: "PATCH", Pattern: "/api/v1/webhooks/{webhook_id}", Handle: (*Server).updateWebhook},
	{Method: "DELETE", Pattern: "/api/v1/webhooks/{webhook_id}", Handle: (*Server).deleteWebhook},
	{Method: "POST", Pattern: "/api/v1/webhooks/{webhook_id}/test", Handle: (*Server).testWebhook},
	{Method: "GET", Pattern: "/api/v1/webhooks/{webhook_id}/deliveries", Handle: (*Server).listDeliveries},
}

// requestTimeout bounds non-streaming handlers.
const requestTimeout = 30 * time.Second

func New(o Options) *Server {
	s := &Server{
		db:        o.DB,
		auth:      newAuthenticator(o.APIKeys),
		version:   o.Version,
		macOS:     o.MacOSVersion,
		startedAt: time.Now().UTC(),
		present:   &present.Presenter{DB: o.DB, Names: o.Names, Dims: o.Dims},
		converter: o.Converter,
		roots:     o.AttachmentRoots,
		bus:       o.Bus,
		store:     o.Store,
		hooks:     o.Webhooks,
		shutdown:  make(chan struct{}),
		sender:    o.Sender,
		uploadMax: o.UploadMaxBytes,
	}
	if s.uploadMax <= 0 {
		s.uploadMax = 100 << 20
	}
	if o.SendsPerMinute <= 0 {
		o.SendsPerMinute = 60
	}
	if o.StreamsPerKey <= 0 {
		o.StreamsPerKey = 20
	}
	s.sendLimit = newRateLimiter(o.SendsPerMinute, 20)
	s.streams = &connLimiter{max: o.StreamsPerKey, count: map[string]int{}}
	if len(s.roots) == 0 {
		if home, err := userHome(); err == nil {
			s.roots = []string{
				filepath.Join(home, "Library", "Messages", "Attachments"),
				filepath.Join(home, "Library", "Messages", "StickerCache"),
			}
		}
	}

	mux := http.NewServeMux()
	byPattern := map[string][]route{}
	var order []string
	for _, rt := range routes {
		if _, ok := byPattern[rt.Pattern]; !ok {
			order = append(order, rt.Pattern)
		}
		byPattern[rt.Pattern] = append(byPattern[rt.Pattern], rt)
	}
	// Patterns are registered without a method so we can answer other
	// methods with a JSON 405 instead of net/http's plain-text one.
	for _, p := range order {
		mux.Handle(p, s.dispatch(byPattern[p]))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, codeNotFound, "no such endpoint")
	})
	s.handler = recoverer(logRequests(cors(o.CORSOrigins, mux)))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// HasKey reports whether an API key name is currently configured.
func (s *Server) HasKey(name string) bool { return s.auth.has(name) }

// SetAPIKeys replaces the accepted API keys without a restart.
func (s *Server) SetAPIKeys(keys []config.APIKey) { s.auth.set(keys) }

// Shutdown closes long-lived connections (WebSockets), which
// http.Server.Shutdown does not track once upgraded.
func (s *Server) Shutdown() { s.closeOnce.Do(func() { close(s.shutdown) }) }

func (s *Server) dispatch(rts []route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.Method
		if method == http.MethodHead {
			method = http.MethodGet
		}
		var rt *route
		allowed := ""
		for i := range rts {
			if allowed != "" {
				allowed += ", "
			}
			allowed += rts[i].Method
			if rts[i].Method == method {
				rt = &rts[i]
			}
		}
		if rt == nil {
			w.Header().Set("Allow", allowed)
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, r.Method+" not allowed")
			return
		}
		if !rt.Public {
			name, ok := s.auth.check(r, rt.TokenQuery)
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="imsgd"`)
				writeError(w, http.StatusUnauthorized, codeUnauthorized, "missing or invalid API key")
				return
			}
			setKeyName(r, name)
		}
		// Go rejects raw ";" in query strings and r.URL.Query() then silently
		// drops every parameter. Chat IDs contain ";", so fail loudly.
		if _, err := url.ParseQuery(r.URL.RawQuery); err != nil {
			writeError(w, http.StatusBadRequest, codeBadRequest,
				"invalid query string: percent-encode reserved characters (e.g. ';' as %3B, '+' as %2B)")
			return
		}
		// Bound how long a client may take to send its request, so slow
		// uploads can't pin connections. WebSockets manage their own.
		rc := http.NewResponseController(w)
		switch {
		case !rt.Stream:
			rc.SetReadDeadline(time.Now().Add(requestTimeout))
			ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
			defer cancel()
			r = r.WithContext(ctx)
		case rt.Method == http.MethodPost:
			rc.SetReadDeadline(time.Now().Add(10 * time.Minute))
		}
		if err := rt.Handle(s, w, r); err != nil {
			writeErr(w, r, err)
		}
	})
}
