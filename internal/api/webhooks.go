package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/matthewmgamble/imsgd/internal/events"
	"github.com/matthewmgamble/imsgd/internal/store"
	"github.com/matthewmgamble/imsgd/internal/webhook"
)

// WebhookDispatcher is the subset of *webhook.Dispatcher the API uses.
type WebhookDispatcher interface {
	Invalidate()
	SendTest(ctx context.Context, w *store.Webhook) webhook.TestResult
}

type webhookJSON struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	Events      []string  `json:"events"`
	ChatIDs     []string  `json:"chat_ids"`
	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Secret      *string   `json:"secret,omitempty"` // only when created
}

func webhookDTO(w *store.Webhook, withSecret bool) webhookJSON {
	out := webhookJSON{
		ID: w.ID, URL: w.URL, Events: w.Events, ChatIDs: w.ChatIDs, Description: w.Description,
		Enabled: w.Enabled, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}
	if withSecret {
		out.Secret = &w.Secret
	}
	return out
}

type deliveryJSON struct {
	ID             string     `json:"id"`
	EventID        string     `json:"event_id"`
	EventType      string     `json:"event_type"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	NextAttemptAt  *time.Time `json:"next_attempt_at"`
	LastStatusCode *int       `json:"last_status_code"`
	LastError      *string    `json:"last_error"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

const maxBody = 64 << 10

// decodeBody strictly decodes a JSON request body.
func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

func validateWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return badRequest("url must be an absolute http or https URL")
	}
	return nil
}

func (s *Server) webhooksEnabled() error {
	if s.store == nil || s.hooks == nil {
		return &apiError{http.StatusServiceUnavailable, codeInternal, "webhooks are not enabled"}
	}
	return nil
}

func (s *Server) getWebhookParam(r *http.Request) (*store.Webhook, error) {
	if err := s.webhooksEnabled(); err != nil {
		return nil, err
	}
	w, err := s.store.GetWebhook(r.Context(), r.PathValue("webhook_id"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && w.Owner != keyNameOf(r)) {
		return nil, notFound("webhook") // webhooks are private to the key that created them
	}
	return w, err
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) error {
	if err := s.webhooksEnabled(); err != nil {
		return err
	}
	hooks, err := s.store.ListWebhooks(r.Context())
	if err != nil {
		return err
	}
	data := make([]webhookJSON, 0, len(hooks))
	for _, h := range hooks {
		if h.Owner == keyNameOf(r) {
			data = append(data, webhookDTO(h, false))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
	return nil
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) error {
	if err := s.webhooksEnabled(); err != nil {
		return err
	}
	var body struct {
		URL         string   `json:"url"`
		Events      []string `json:"events"`
		ChatIDs     []string `json:"chat_ids"`
		Description string   `json:"description"`
		Enabled     *bool    `json:"enabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		return err
	}
	if err := validateWebhookURL(body.URL); err != nil {
		return err
	}
	if err := events.ValidateTypes(body.Events); err != nil {
		return badRequest("events: %v", err)
	}
	hook := &store.Webhook{Owner: keyNameOf(r), URL: body.URL, Events: body.Events, ChatIDs: body.ChatIDs, Description: body.Description, Enabled: true}
	if body.Enabled != nil {
		hook.Enabled = *body.Enabled
	}
	if err := s.store.CreateWebhook(r.Context(), hook); err != nil {
		return err
	}
	s.hooks.Invalidate()
	writeJSON(w, http.StatusCreated, webhookDTO(hook, true))
	return nil
}

func (s *Server) getWebhook(w http.ResponseWriter, r *http.Request) error {
	hook, err := s.getWebhookParam(r)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, webhookDTO(hook, false))
	return nil
}

func (s *Server) updateWebhook(w http.ResponseWriter, r *http.Request) error {
	hook, err := s.getWebhookParam(r)
	if err != nil {
		return err
	}
	var body struct {
		URL         *string   `json:"url"`
		Events      *[]string `json:"events"`
		ChatIDs     *[]string `json:"chat_ids"`
		Description *string   `json:"description"`
		Enabled     *bool     `json:"enabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		return err
	}
	if body.URL != nil {
		if err := validateWebhookURL(*body.URL); err != nil {
			return err
		}
	}
	if body.Events != nil {
		if err := events.ValidateTypes(*body.Events); err != nil {
			return badRequest("events: %v", err)
		}
	}
	updated, err := s.store.UpdateWebhook(r.Context(), hook.ID, store.WebhookPatch{
		URL: body.URL, Events: body.Events, ChatIDs: body.ChatIDs, Description: body.Description, Enabled: body.Enabled,
	})
	if err != nil {
		return err
	}
	s.hooks.Invalidate()
	// A URL change rotates the secret; return the new one (only time it's shown).
	writeJSON(w, http.StatusOK, webhookDTO(updated, updated.Secret != hook.Secret))
	return nil
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) error {
	hook, err := s.getWebhookParam(r)
	if err != nil {
		return err
	}
	if err := s.store.DeleteWebhook(r.Context(), hook.ID); err != nil {
		return err
	}
	s.hooks.Invalidate()
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) testWebhook(w http.ResponseWriter, r *http.Request) error {
	hook, err := s.getWebhookParam(r)
	if err != nil {
		return err
	}
	res := s.hooks.SendTest(r.Context(), hook)
	var code *int
	if res.StatusCode != 0 {
		code = &res.StatusCode
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": res.OK, "status_code": code, "error": strPtrOrNil(res.Error), "duration_ms": res.Duration.Milliseconds(),
	})
	return nil
}

func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request) error {
	hook, err := s.getWebhookParam(r)
	if err != nil {
		return err
	}
	limit, err := queryLimit(r)
	if err != nil {
		return err
	}
	list, err := s.store.ListDeliveries(r.Context(), hook.ID, limit)
	if err != nil {
		return err
	}
	data := make([]deliveryJSON, 0, len(list))
	for _, d := range list {
		data = append(data, deliveryJSON{
			ID: d.ID, EventID: d.EventID, EventType: d.EventType, Status: string(d.Status), Attempts: d.Attempts,
			NextAttemptAt: d.NextAttemptAt, LastStatusCode: d.LastStatusCode, LastError: d.LastError,
			CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
	return nil
}
