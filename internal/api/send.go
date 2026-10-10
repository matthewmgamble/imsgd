package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/matthewmgamble/imsgd/internal/imessage/db"
	"github.com/matthewmgamble/imsgd/internal/present"
	"github.com/matthewmgamble/imsgd/internal/send"
	"github.com/matthewmgamble/imsgd/internal/store"
)

const (
	maxTextLen     = 20000
	maxAttachments = 10
	sendWait       = 60 * time.Second
)

type sendBody struct {
	ChatID    string `json:"chat_id"`
	To        string `json:"to"`
	Service   string `json:"service"`
	Text      string `json:"text"`
	ClientRef string `json:"client_ref"`
	Wait      *bool  `json:"wait"`
}

func (s *Server) sendingEnabled() error {
	if s.sender == nil {
		return &apiError{http.StatusServiceUnavailable, codeInternal, "sending is not enabled"}
	}
	return nil
}

func (s *Server) createSend(w http.ResponseWriter, r *http.Request) error {
	if err := s.sendingEnabled(); err != nil {
		return err
	}
	keyName := keyNameOf(r)
	if ok, retry := s.sendLimit.allow(keyName); !ok {
		tooManyRequests(w, "send rate limit exceeded for this API key", retry)
		return nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.uploadMax)

	var body sendBody
	var files []*multipart.FileHeader
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch mt {
	case "application/json":
		dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			return badRequest("invalid JSON body: %v", err)
		}
	case "multipart/form-data":
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				return &apiError{http.StatusRequestEntityTooLarge, codeTooLarge, fmt.Sprintf("request exceeds %d bytes", s.uploadMax)}
			}
			return badRequest("invalid multipart body: %v", err)
		}
		defer r.MultipartForm.RemoveAll()
		if p := r.FormValue("payload"); p != "" {
			dec := json.NewDecoder(strings.NewReader(p))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&body); err != nil {
				return badRequest("invalid payload field: %v", err)
			}
		}
		files = r.MultipartForm.File["attachments"]
	default:
		return badRequest("Content-Type must be application/json or multipart/form-data")
	}

	if err := s.validateSend(r.Context(), &body, len(files)); err != nil {
		return err
	}
	wait := body.Wait == nil || *body.Wait

	// Idempotency: a repeated client_ref returns the original send without
	// staging files or sending again.
	if body.ClientRef != "" {
		if existing, err := s.store.GetSendByClientRef(r.Context(), keyName, body.ClientRef); err == nil {
			w.Header().Set("Idempotent-Replay", "true")
			return s.respondSend(w, r, existing.ID, false, true)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}

	id := store.NewSendID()
	var staged []send.StagedFile
	for i, fh := range files {
		path, err := s.stage(id, i, fh)
		if err != nil {
			return err
		}
		staged = append(staged, send.StagedFile{Name: fh.Filename, Path: path})
	}

	snd, created, err := s.sender.Submit(r.Context(), send.Request{
		ID: id, APIKey: keyName, ClientRef: body.ClientRef, ChatID: body.ChatID, To: body.To,
		Service: send.Service(body.Service), Text: body.Text, Files: staged,
	})
	if err != nil {
		return err
	}
	if !created {
		w.Header().Set("Idempotent-Replay", "true")
	}
	return s.respondSend(w, r, snd.ID, wait && created, !created)
}

func (s *Server) validateSend(ctx context.Context, b *sendBody, nFiles int) error {
	b.ChatID, b.To = strings.TrimSpace(b.ChatID), strings.TrimSpace(b.To)
	switch {
	case (b.ChatID == "") == (b.To == ""):
		return badRequest("exactly one of chat_id or to is required")
	case strings.TrimSpace(b.Text) == "" && nFiles == 0:
		return badRequest("text or at least one attachment is required")
	case utf8.RuneCountInString(b.Text) > maxTextLen:
		return badRequest("text exceeds %d characters", maxTextLen)
	case nFiles > maxAttachments:
		return badRequest("at most %d attachments per send", maxAttachments)
	case len(b.ClientRef) > 200:
		return badRequest("client_ref exceeds 200 characters")
	}
	if b.To != "" {
		if !send.ValidAddress(b.To) {
			return badRequest("to must be a phone number or email address")
		}
		switch b.Service {
		case "":
			b.Service = string(send.ServiceIMessage)
		case string(send.ServiceIMessage), string(send.ServiceSMS):
		default:
			return badRequest("service must be iMessage or SMS")
		}
	} else {
		if b.Service != "" {
			return badRequest("service can only be used with to (a chat already has a service)")
		}
		if _, err := s.db.GetChat(ctx, b.ChatID); errors.Is(err, db.ErrNotFound) {
			return notFound("chat")
		} else if err != nil {
			return err
		}
	}
	return nil
}

// stage copies an uploaded file into the permanent staging directory.
func (s *Server) stage(sendID string, i int, fh *multipart.FileHeader) (string, error) {
	src, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer src.Close()
	path := s.sender.StagePath(sendID, i, fh.Filename)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(path)
		return "", err
	}
	return path, dst.Close()
}

// respondSend writes {send, messages}, optionally waiting for the send to
// settle. For a new send the status is 201 when every part is matched or
// failed, otherwise 202; lookups and idempotent replays use 200.
func (s *Server) respondSend(w http.ResponseWriter, r *http.Request, id string, wait, lookup bool) error {
	var snd *store.Send
	var err error
	if wait {
		ctx, cancel := context.WithTimeout(r.Context(), sendWait)
		snd, err = s.sender.Wait(ctx, id)
		cancel()
	} else {
		snd, err = s.sender.Get(r.Context(), id)
	}
	if err != nil {
		return err
	}
	var guids []string
	for _, it := range snd.Items {
		if it.MessageID != "" {
			guids = append(guids, it.MessageID)
		}
	}
	msgs := []present.Message{}
	if len(guids) > 0 {
		var ms []*db.Message
		for _, g := range guids {
			m, err := s.db.GetMessage(r.Context(), g)
			if err == nil {
				ms = append(ms, m)
			} else if !errors.Is(err, db.ErrNotFound) {
				return err
			}
		}
		if msgs, err = s.present.Messages(r.Context(), ms); err != nil {
			return err
		}
	}
	status := http.StatusAccepted
	switch {
	case lookup:
		status = http.StatusOK
	case send.Settled(snd):
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"send": present.SendOf(snd), "messages": msgs})
	return nil
}

func (s *Server) getSend(w http.ResponseWriter, r *http.Request) error {
	if err := s.sendingEnabled(); err != nil {
		return err
	}
	snd, err := s.sender.Get(r.Context(), r.PathValue("send_id"))
	if errors.Is(err, store.ErrNotFound) {
		return notFound("send")
	}
	if err != nil {
		return err
	}
	if snd.APIKey != keyNameOf(r) {
		return notFound("send") // sends are private to the key that created them
	}
	return s.respondSend(w, r, snd.ID, false, true)
}
