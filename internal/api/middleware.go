package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/matthewmgamble/imsgd/internal/config"
)

// authenticator checks bearer tokens against the configured API keys.
// Keys are compared as SHA-256 digests in constant time, and every key is
// checked so timing does not reveal which one matched.
type authenticator struct {
	mu   sync.RWMutex
	keys []hashedKey
}

type hashedKey struct {
	name   string
	digest [32]byte
}

func newAuthenticator(keys []config.APIKey) *authenticator {
	a := &authenticator{}
	a.set(keys)
	return a
}

// set replaces the accepted keys (hot reload).
func (a *authenticator) set(keys []config.APIKey) {
	hashed := make([]hashedKey, 0, len(keys))
	for _, k := range keys {
		hashed = append(hashed, hashedKey{name: k.Name, digest: sha256.Sum256([]byte(k.Key))})
	}
	a.mu.Lock()
	a.keys = hashed
	a.mu.Unlock()
}

// has reports whether a key with this name is configured.
func (a *authenticator) has(name string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, k := range a.keys {
		if k.name == name {
			return true
		}
	}
	return false
}

// check authenticates r. allowQuery also accepts ?token= (WebSocket only).
func (a *authenticator) check(r *http.Request, allowQuery bool) (string, bool) {
	token := ""
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, value, ok := strings.Cut(h, " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			token = strings.TrimSpace(value)
		}
	}
	if token == "" && allowQuery {
		token = r.URL.Query().Get("token")
	}
	if token == "" {
		return "", false
	}
	d := sha256.Sum256([]byte(token))
	match := ""
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, k := range a.keys {
		if subtle.ConstantTimeCompare(d[:], k.digest[:]) == 1 {
			match = k.name
		}
	}
	return match, match != ""
}

// requestInfo is shared between the logging middleware and handlers.
type requestInfo struct {
	keyName string
}

type ctxKey struct{}

// keyNameOf returns the name of the API key that authenticated r.
func keyNameOf(r *http.Request) string {
	if info, ok := r.Context().Value(ctxKey{}).(*requestInfo); ok {
		return info.keyName
	}
	return ""
}

func setKeyName(r *http.Request, name string) {
	if info, ok := r.Context().Value(ctxKey{}).(*requestInfo); ok {
		info.keyName = name
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests logs one line per request. Query strings are not logged.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &requestInfo{}
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, info))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		level := slog.LevelInfo
		if sw.status >= 500 {
			level = slog.LevelError
		} else if r.URL.Path == "/health" {
			level = slog.LevelDebug
		}
		slog.Log(r.Context(), level, "http",
			"method", r.Method, "path", r.URL.Path, "status", sw.status,
			"bytes", sw.bytes, "dur", time.Since(start).Round(time.Microsecond),
			"key", info.keyName, "remote", r.RemoteAddr)
	})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				slog.Error("panic", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, codeInternal, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func userHome() (string, error) { return os.UserHomeDir() }

// cors answers preflight requests and adds CORS headers for allowed origins.
// With no configured origins, no CORS headers are sent (browsers on other
// origins cannot call the API; WebSocket is unaffected).
func cors(origins []string, next http.Handler) http.Handler {
	if len(origins) == 0 {
		return next
	}
	allowAll := false
	allowed := map[string]bool{}
	for _, o := range origins {
		if o == "*" {
			allowAll = true
		}
		allowed[strings.TrimRight(o, "/")] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (allowAll || allowed[origin]) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", "Idempotent-Replay, Content-Disposition")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Range")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
