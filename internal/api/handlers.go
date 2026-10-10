package api

import (
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	apispec "github.com/matthewmgamble/imsgd/api"
	"github.com/matthewmgamble/imsgd/internal/imessage/db"
	"github.com/matthewmgamble/imsgd/internal/media"
	"github.com/matthewmgamble/imsgd/internal/present"
)

func (s *Server) getHealth(w http.ResponseWriter, r *http.Request) error {
	checks := map[string]string{"messages_db": "ok"}
	status, code := "ok", http.StatusOK
	if _, err := s.db.MaxMessageRowID(r.Context()); err != nil {
		slog.Warn("health check: Messages database unavailable", "err", err)
		checks["messages_db"] = "unavailable" // details are logged, not exposed on this public endpoint
		status, code = "degraded", http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"status": status, "checks": checks})
	return nil
}

func (s *Server) getServer(w http.ResponseWriter, r *http.Request) error {
	maxRow, err := s.db.MaxMessageRowID(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":       s.version,
		"macos_version": s.macOS,
		"started_at":    s.startedAt,
		"messages_db":   map[string]any{"max_rowid": maxRow},
	})
	return nil
}

func (s *Server) getOpenAPI(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "application/yaml")
	_, err := w.Write(apispec.Spec)
	return err
}

// ---- chats -----------------------------------------------------------------

func (s *Server) listChats(w http.ResponseWriter, r *http.Request) error {
	limit, err := queryLimit(r)
	if err != nil {
		return err
	}
	inc, err := queryInclude(r, "participants", "last_message")
	if err != nil {
		return err
	}
	cur, err := decodeCursor(r, "chats")
	if err != nil {
		return err
	}
	q := db.ChatsQuery{Limit: limit + 1, Search: r.URL.Query().Get("q"), IncludeParticipants: inc["participants"]}
	if cur != nil {
		q.Before = &db.ChatCursor{LastDate: cur.Date, RowID: cur.RowID}
	}
	chats, err := s.db.ListChats(r.Context(), q)
	if err != nil {
		return err
	}
	var next *string
	if len(chats) > limit {
		chats = chats[:limit]
		c := db.ChatCursorFor(chats[limit-1])
		next = encodeCursor(cursor{Kind: "chats", Date: c.LastDate, RowID: c.RowID})
	}
	data, err := s.chatDTOs(r, chats, inc["participants"], inc["last_message"])
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "next_cursor": next})
	return nil
}

func (s *Server) chatDTOs(r *http.Request, chats []*db.Chat, participants, lastMessage bool) ([]present.Chat, error) {
	out := make([]present.Chat, 0, len(chats))
	for _, c := range chats {
		out = append(out, s.present.Chat(c, participants))
	}
	if !lastMessage || len(chats) == 0 {
		return out, nil
	}
	ids := make([]int64, len(chats))
	for i, c := range chats {
		ids[i] = c.RowID
	}
	last, err := s.db.LastMessages(r.Context(), ids)
	if err != nil {
		return nil, err
	}
	var msgs []*db.Message
	for _, c := range chats {
		if m := last[c.RowID]; m != nil {
			msgs = append(msgs, m)
		}
	}
	dtos, err := s.present.Messages(r.Context(), msgs)
	if err != nil {
		return nil, err
	}
	byGUID := map[string]*present.Message{}
	for i := range dtos {
		byGUID[dtos[i].ID] = &dtos[i]
	}
	for i, c := range chats {
		var mj *present.Message
		if m := last[c.RowID]; m != nil {
			mj = byGUID[m.GUID]
		}
		out[i].LastMessage = &mj
	}
	return out, nil
}

func (s *Server) getChat(w http.ResponseWriter, r *http.Request) error {
	inc, err := queryInclude(r, "participants", "last_message")
	if err != nil {
		return err
	}
	c, err := s.db.GetChat(r.Context(), r.PathValue("chat_id"))
	if errors.Is(err, db.ErrNotFound) {
		return notFound("chat")
	}
	if err != nil {
		return err
	}
	data, err := s.chatDTOs(r, []*db.Chat{c}, true, inc["last_message"])
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, data[0])
	return nil
}

func (s *Server) listChatMessages(w http.ResponseWriter, r *http.Request) error {
	chatID := r.PathValue("chat_id")
	limit, err := queryLimit(r)
	if err != nil {
		return err
	}
	order := r.URL.Query().Get("order")
	switch order {
	case "":
		order = "desc"
	case "asc", "desc":
	default:
		return badRequest("order must be asc or desc")
	}
	kind := "chat_messages:" + chatID
	cur, err := decodeCursor(r, kind)
	if err != nil {
		return err
	}
	if cur != nil && cur.Order != order {
		return badRequest("cursor was issued for order=%s", cur.Order)
	}
	q := db.ChatMessagesQuery{ChatGUID: chatID, Limit: limit + 1, Ascending: order == "asc"}
	if cur != nil {
		c := &db.Cursor{Date: cur.Date, RowID: cur.RowID}
		if q.Ascending {
			q.After = c
		} else {
			q.Before = c
		}
	}
	msgs, err := s.db.ChatMessages(r.Context(), q)
	if errors.Is(err, db.ErrNotFound) {
		return notFound("chat")
	}
	if err != nil {
		return err
	}
	var next *string
	if len(msgs) > limit {
		msgs = msgs[:limit]
		c := db.CursorFor(msgs[limit-1])
		next = encodeCursor(cursor{Kind: kind, Date: c.Date, RowID: c.RowID, Order: order})
	}
	data, err := s.present.Messages(r.Context(), msgs)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "next_cursor": next})
	return nil
}

// ---- messages ----------------------------------------------------------------

func (s *Server) syncMessages(w http.ResponseWriter, r *http.Request) error {
	since, err := queryInt64(r, "since_id", 0)
	if err != nil {
		return err
	}
	limit, err := queryLimit(r)
	if err != nil {
		return err
	}
	msgs, err := s.db.MessagesSince(r.Context(), since, r.URL.Query().Get("chat_id"), limit+1)
	if err != nil {
		return err
	}
	hasMore := len(msgs) > limit
	if hasMore {
		msgs = msgs[:limit]
	}
	next := since
	if len(msgs) > 0 {
		next = msgs[len(msgs)-1].RowID
	}
	data, err := s.present.Messages(r.Context(), msgs)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "next_since_id": next, "has_more": hasMore})
	return nil
}

func (s *Server) getMessage(w http.ResponseWriter, r *http.Request) error {
	m, err := s.db.GetMessage(r.Context(), r.PathValue("message_id"))
	if errors.Is(err, db.ErrNotFound) {
		return notFound("message")
	}
	if err != nil {
		return err
	}
	data, err := s.present.Messages(r.Context(), []*db.Message{m})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, data[0])
	return nil
}

// ---- handles -----------------------------------------------------------------

func (s *Server) listHandles(w http.ResponseWriter, r *http.Request) error {
	limit, err := queryLimit(r)
	if err != nil {
		return err
	}
	cur, err := decodeCursor(r, "handles")
	if err != nil {
		return err
	}
	q := db.HandlesQuery{Search: r.URL.Query().Get("q"), Limit: limit + 1}
	if cur != nil {
		q.AfterRowID = cur.RowID
	}
	hs, err := s.db.ListHandles(r.Context(), q)
	if err != nil {
		return err
	}
	var next *string
	if len(hs) > limit {
		hs = hs[:limit]
		next = encodeCursor(cursor{Kind: "handles", RowID: hs[limit-1].RowID})
	}
	data := make([]present.Handle, 0, len(hs))
	for _, h := range hs {
		data = append(data, s.present.Handle(h))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "next_cursor": next})
	return nil
}

// ---- attachments -------------------------------------------------------------

func (s *Server) lookupAttachment(r *http.Request) (*db.Attachment, string, error) {
	a, err := s.db.GetAttachment(r.Context(), r.PathValue("attachment_id"))
	if errors.Is(err, db.ErrNotFound) || (err == nil && a.HideAttachment) {
		return nil, "", notFound("attachment")
	}
	if err != nil {
		return nil, "", err
	}
	msgGUID := ""
	if a.MessageRowID != 0 {
		if msgGUID, err = s.db.MessageGUID(r.Context(), a.MessageRowID); err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, "", err
		}
	}
	return a, msgGUID, nil
}

func (s *Server) getAttachment(w http.ResponseWriter, r *http.Request) error {
	a, msgGUID, err := s.lookupAttachment(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.present.Attachment(a, msgGUID))
	return nil
}

func (s *Server) getAttachmentContent(w http.ResponseWriter, r *http.Request) error {
	a, _, err := s.lookupAttachment(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	format := q.Get("format")
	if format == "" {
		format = "original"
	}
	maxSize := 0
	if v := q.Get("max_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 16 || n > 8192 {
			return badRequest("max_size must be an integer between 16 and 8192")
		}
		if format != "jpeg" {
			return badRequest("max_size requires format=jpeg")
		}
		maxSize = sizeBucket(n)
	}
	switch format {
	case "original", "jpeg", "m4a", "live":
	default:
		return badRequest("format must be original, jpeg, m4a or live")
	}

	unavailable := &apiError{http.StatusNotFound, codeAttachmentUnavailable,
		"attachment file is not on this Mac (not downloaded or offloaded to iCloud)"}
	if !present.AttachmentAvailable(a) {
		return unavailable
	}
	path, err := s.safePath(a.Filename)
	if err != nil {
		return err
	}
	name := a.TransferName
	if name == "" {
		name = filepath.Base(path)
	}
	contentType := a.MimeType

	switch format {
	case "live":
		mov := media.LivePhotoVideo(path)
		if mov == "" || !strings.HasPrefix(a.MimeType, "image/") {
			return &apiError{http.StatusNotFound, codeAttachmentUnavailable, "this attachment has no Live Photo video"}
		}
		if path, err = s.safePath(mov); err != nil {
			return err
		}
		contentType = "video/quicktime"
		name = strings.TrimSuffix(name, filepath.Ext(name)) + ".mov"
	case "jpeg", "m4a":
		f := media.Format(format)
		if !media.CanConvert(a.MimeType, a.UTI, f) {
			return badRequest("cannot convert %s to %s", a.MimeType, format)
		}
		if s.converter == nil {
			return &apiError{http.StatusServiceUnavailable, codeInternal, "conversion is not enabled"}
		}
		converted, err := s.converter.Convert(r.Context(), path, f, maxSize)
		if err != nil {
			slog.Warn("attachment conversion failed", "attachment", a.GUID, "format", format, "err", err)
			return &apiError{http.StatusUnprocessableEntity, codeConversionFailed, "could not convert this attachment to " + format}
		}
		path = converted
		contentType = f.ContentType()
		name = strings.TrimSuffix(name, filepath.Ext(name)) + map[string]string{"jpeg": ".jpg", "m4a": ".m4a"}[format]
	}

	f, err := os.Open(path)
	if err != nil {
		return unavailable
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	// Attachments come from arbitrary senders. Only passive media types are
	// shown inline; anything a browser could execute (HTML, SVG, XML, JS…)
	// is served as a download with a neutral type, and the sandbox CSP stops
	// scripts even if a client renders it anyway.
	disposition := "attachment"
	if inlineSafe(contentType) {
		disposition = "inline"
	} else if activeContent(contentType) {
		contentType = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, name, fi.ModTime(), f)
	return nil
}

func inlineSafe(ct string) bool {
	switch ct {
	case "image/jpeg", "image/png", "image/gif", "image/heic", "image/heif", "image/webp", "image/avif", "image/tiff":
		return true
	}
	return strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "audio/")
}

func activeContent(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.Contains(ct, "html") || strings.Contains(ct, "svg") || strings.Contains(ct, "xml") ||
		strings.Contains(ct, "javascript") || strings.Contains(ct, "ecmascript") || ct == "text/css"
}

// safePath resolves symlinks and ensures the file lies under an allowed root,
// so a crafted chat.db path cannot expose arbitrary files.
func (s *Server) safePath(p string) (string, error) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", &apiError{http.StatusNotFound, codeAttachmentUnavailable, "attachment file is not available"}
	}
	for _, root := range s.roots {
		rr, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(rr, real); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return real, nil
		}
	}
	slog.Warn("refusing to serve attachment outside allowed roots", "path", p)
	return "", &apiError{http.StatusNotFound, codeAttachmentUnavailable, "attachment file is not available"}
}

// sizeBuckets bound the number of distinct conversions per image: a
// requested max_size is rounded up to the next bucket.
var sizeBuckets = []int{64, 128, 256, 320, 480, 640, 800, 1024, 1280, 1600, 2048, 3072, 4096, 8192}

func sizeBucket(n int) int {
	for _, b := range sizeBuckets {
		if n <= b {
			return b
		}
	}
	return sizeBuckets[len(sizeBuckets)-1]
}
