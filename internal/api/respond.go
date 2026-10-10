package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/matthewmgamble/imsgd/internal/imessage/db"
)

// Error codes (see Error.code in openapi.yaml).
const (
	codeBadRequest            = "bad_request"
	codeUnauthorized          = "unauthorized"
	codeNotFound              = "not_found"
	codeAttachmentUnavailable = "attachment_unavailable"
	codeMethodNotAllowed      = "method_not_allowed"
	codeTooLarge              = "payload_too_large"
	codeConversionFailed      = "conversion_failed"
	codeRateLimited           = "rate_limited"
	codeInternal              = "internal"
)

// apiError is an error with an HTTP status and code.
type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, a ...any) error {
	return &apiError{http.StatusBadRequest, codeBadRequest, fmt.Sprintf(format, a...)}
}

func notFound(what string) error {
	return &apiError{http.StatusNotFound, codeNotFound, what + " not found"}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		slog.Debug("write response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// writeErr maps an error to a response. Unexpected errors are logged and
// reported as 500 without internal details.
func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		writeError(w, ae.status, ae.code, ae.msg)
	case errors.Is(err, db.ErrNotFound):
		writeError(w, http.StatusNotFound, codeNotFound, "not found")
	default:
		slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, codeInternal, "internal server error")
	}
}

// ---- parameters ------------------------------------------------------------

const maxPageSize = 1000

func queryLimit(r *http.Request) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return db.DefaultLimit, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxPageSize {
		return 0, badRequest("limit must be an integer between 1 and %d", maxPageSize)
	}
	return n, nil
}

func queryInt64(r *http.Request, name string, def int64) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, badRequest("%s must be a non-negative integer", name)
	}
	return n, nil
}

// queryInclude parses a comma-separated include list against allowed values.
func queryInclude(r *http.Request, allowed ...string) (map[string]bool, error) {
	out := map[string]bool{}
	v := r.URL.Query().Get("include")
	if v == "" {
		return out, nil
	}
	for _, item := range strings.Split(v, ",") {
		item = strings.TrimSpace(item)
		ok := false
		for _, a := range allowed {
			if item == a {
				ok = true
			}
		}
		if !ok {
			return nil, badRequest("include: unknown value %q (allowed: %s)", item, strings.Join(allowed, ", "))
		}
		out[item] = true
	}
	return out, nil
}

// ---- cursors -----------------------------------------------------------------

// cursor is the opaque pagination token. Kind binds it to one listing so a
// cursor from one endpoint cannot be replayed against another.
type cursor struct {
	Kind  string `json:"k"`
	Date  int64  `json:"d,omitempty"`
	RowID int64  `json:"r"`
	Order string `json:"o,omitempty"`
}

func encodeCursor(c cursor) *string {
	b, _ := json.Marshal(c)
	s := base64.RawURLEncoding.EncodeToString(b)
	return &s
}

func decodeCursor(r *http.Request, kind string) (*cursor, error) {
	v := r.URL.Query().Get("cursor")
	if v == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(v)
	var c cursor
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil || c.Kind != kind {
		return nil, badRequest("invalid cursor")
	}
	return &c, nil
}
