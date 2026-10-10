package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

func open(t *testing.T) *Store {
	s, err := Open(filepath.Join(t.TempDir(), "sub", "imsgd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWebhookCRUD(t *testing.T) {
	s := open(t)
	w := &Webhook{URL: "http://x.local/hook", Events: []string{"message.*"}, Enabled: true}
	if err := s.CreateWebhook(ctx, w); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(w.ID, "wh_") || !strings.HasPrefix(w.Secret, "whsec_") || w.ChatIDs == nil {
		t.Fatalf("created = %+v", w)
	}
	got, err := s.GetWebhook(ctx, w.ID)
	if err != nil || got.URL != w.URL || got.Secret != w.Secret || len(got.Events) != 1 || !got.Enabled || !got.CreatedAt.Equal(w.CreatedAt) {
		t.Fatalf("get = %+v, %v", got, err)
	}
	off := false
	desc := "test"
	up, err := s.UpdateWebhook(ctx, w.ID, WebhookPatch{Enabled: &off, Description: &desc})
	if err != nil || up.Enabled || up.Description != "test" || up.URL != w.URL {
		t.Fatalf("update = %+v, %v", up, err)
	}
	list, _ := s.ListWebhooks(ctx)
	if len(list) != 1 || list[0].Enabled {
		t.Fatalf("list = %+v", list)
	}
	if err := s.DeleteWebhook(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWebhook(ctx, w.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := s.DeleteWebhook(ctx, w.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("double delete should be not found")
	}
}

func TestDeliveries(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	w := &Webhook{URL: "http://x/h", Enabled: true}
	s.CreateWebhook(ctx, w)

	d, err := s.EnqueueDelivery(ctx, w.ID, "evt-1", "message.received", []byte(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	due, _ := s.DueDeliveries(ctx, 10)
	if len(due) != 1 || due[0].ID != d.ID || string(due[0].Payload) != `{"a":1}` {
		t.Fatalf("due = %+v", due)
	}

	// Retry in 10s: not due now, due later.
	next := now.Add(10 * time.Second)
	code := 500
	msg := "server error"
	s.RecordAttempt(ctx, d.ID, DeliveryPending, 1, &next, &code, &msg)
	if due, _ := s.DueDeliveries(ctx, 10); len(due) != 0 {
		t.Fatal("should not be due yet")
	}
	if at, _ := s.NextDueAt(ctx); at == nil || !at.Equal(next) {
		t.Fatalf("next due = %v", at)
	}
	now = now.Add(11 * time.Second)
	if due, _ := s.DueDeliveries(ctx, 10); len(due) != 1 || due[0].Attempts != 1 || *due[0].LastStatusCode != 500 {
		t.Fatalf("due after wait = %+v", due)
	}
	s.RecordAttempt(ctx, d.ID, DeliverySucceeded, 2, nil, nil, nil)
	if at, _ := s.NextDueAt(ctx); at != nil {
		t.Fatal("nothing should be pending")
	}
	list, _ := s.ListDeliveries(ctx, w.ID, 10)
	if len(list) != 1 || list[0].Status != DeliverySucceeded || list[0].LastError != nil {
		t.Fatalf("list = %+v", list[0])
	}

	// Pruning and cascade delete.
	now = now.Add(8 * 24 * time.Hour)
	if n, _ := s.PruneDeliveries(ctx, now.Add(-7*24*time.Hour)); n != 1 {
		t.Fatalf("pruned %d", n)
	}
	s.EnqueueDelivery(ctx, w.ID, "evt-2", "chat.read", []byte(`{}`))
	s.DeleteWebhook(ctx, w.ID)
	if list, _ := s.ListDeliveries(ctx, w.ID, 10); len(list) != 0 {
		t.Fatal("deliveries not cascaded")
	}
}

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "imsgd.db")
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}

func TestSends(t *testing.T) {
	s := open(t)
	snd := &Send{ID: NewSendID(), APIKey: "app", ClientRef: "ref-1", ChatID: "any;-;x", Status: SendPending,
		Items: []SendItem{{Index: 0, Kind: "text", Text: "hi", Status: SendPending}}}
	if err := s.CreateSend(ctx, snd); err != nil {
		t.Fatal(err)
	}
	dup := &Send{ID: NewSendID(), APIKey: "app", ClientRef: "ref-1", Status: SendPending}
	if err := s.CreateSend(ctx, dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate client_ref: %v", err)
	}
	// Same ref under another key, and sends without a ref, are fine.
	if err := s.CreateSend(ctx, &Send{ID: NewSendID(), APIKey: "other", ClientRef: "ref-1", Status: SendPending}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.CreateSend(ctx, &Send{ID: NewSendID(), APIKey: "app", Status: SendSent}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.GetSendByClientRef(ctx, "app", "ref-1")
	if err != nil || got.ID != snd.ID || len(got.Items) != 1 || got.Items[0].Text != "hi" {
		t.Fatalf("by ref = %+v, %v", got, err)
	}
	snd.Status = SendDelivered
	snd.Items[0].MessageID = "GUID"
	s.UpdateSend(ctx, snd)
	got, _ = s.GetSend(ctx, snd.ID)
	if got.Status != SendDelivered || got.Items[0].MessageID != "GUID" {
		t.Fatalf("updated = %+v", got)
	}
	pending, _ := s.SendsWithStatus(ctx, SendPending)
	if len(pending) != 1 {
		t.Fatalf("pending = %d", len(pending))
	}
	if _, err := s.GetSend(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
