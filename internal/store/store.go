// Package store is imsgd's own SQLite database (webhooks, delivery queue).
// It lives in the data directory, never in ~/Library/Messages.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db  *sql.DB
	now func() time.Time
}

// migrations are applied in order; PRAGMA user_version records progress.
// Append only: never edit or reorder an existing entry.
var migrations = []string{
	`CREATE TABLE webhooks (
		id          TEXT PRIMARY KEY,
		url         TEXT NOT NULL,
		secret      TEXT NOT NULL,
		events      TEXT NOT NULL,             -- JSON array of type filters
		chat_ids    TEXT NOT NULL,             -- JSON array; empty = all chats
		description TEXT NOT NULL DEFAULT '',
		enabled     INTEGER NOT NULL DEFAULT 1,
		created_at  INTEGER NOT NULL,          -- unix ms
		updated_at  INTEGER NOT NULL
	);
	CREATE TABLE webhook_deliveries (
		id               TEXT PRIMARY KEY,
		webhook_id       TEXT NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
		event_id         TEXT NOT NULL,
		event_type       TEXT NOT NULL,
		payload          BLOB NOT NULL,
		status           TEXT NOT NULL,        -- pending, succeeded, failed
		attempts         INTEGER NOT NULL DEFAULT 0,
		next_attempt_at  INTEGER,
		last_status_code INTEGER,
		last_error       TEXT,
		created_at       INTEGER NOT NULL,
		updated_at       INTEGER NOT NULL
	);
	CREATE INDEX webhook_deliveries_due ON webhook_deliveries(status, next_attempt_at);
	CREATE INDEX webhook_deliveries_webhook ON webhook_deliveries(webhook_id, created_at);`,

	// 2: sends (POST /api/v1/messages).
	`CREATE TABLE sends (
		id          TEXT PRIMARY KEY,
		api_key     TEXT NOT NULL,             -- name of the API key that created it
		client_ref  TEXT,                      -- idempotency key, unique per API key
		chat_id     TEXT NOT NULL DEFAULT '',
		recipient   TEXT NOT NULL DEFAULT '',
		service     TEXT NOT NULL DEFAULT '',
		status      TEXT NOT NULL,
		error       TEXT NOT NULL DEFAULT '',
		items       TEXT NOT NULL,             -- JSON []SendItem
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL
	);
	CREATE UNIQUE INDEX sends_client_ref ON sends(api_key, client_ref) WHERE client_ref IS NOT NULL;
	CREATE INDEX sends_status ON sends(status);`,

	// 3: webhooks belong to the API key that created them.
	`ALTER TABLE webhooks ADD COLUMN owner TEXT NOT NULL DEFAULT ''`,
}

// Open opens (creating if needed) the store at path and applies migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Create the file owner-only before SQLite does (it holds webhook secrets
	// and queued event payloads).
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		f.Close()
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite writes are serialized anyway; avoids SQLITE_BUSY
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	os.Chmod(path, 0o600)
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func randomID(prefix string, n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// ---- webhooks ------------------------------------------------------------------

type Webhook struct {
	ID          string
	Owner       string // name of the API key that created it
	URL         string
	Secret      string
	Events      []string
	ChatIDs     []string
	Description string
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewSecret returns a random signing secret.
func NewSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b)
}

// CreateWebhook inserts w, assigning ID, secret and timestamps.
func (s *Store) CreateWebhook(ctx context.Context, w *Webhook) error {
	now := s.now()
	w.ID = randomID("wh_", 8)
	w.Secret = NewSecret()
	w.CreatedAt, w.UpdatedAt = now.UTC().Truncate(time.Millisecond), now.UTC().Truncate(time.Millisecond)
	if w.Events == nil {
		w.Events = []string{}
	}
	if w.ChatIDs == nil {
		w.ChatIDs = []string{}
	}
	ev, _ := json.Marshal(w.Events)
	chats, _ := json.Marshal(w.ChatIDs)
	_, err := s.db.ExecContext(ctx, `INSERT INTO webhooks (id, owner, url, secret, events, chat_ids, description, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, w.ID, w.Owner, w.URL, w.Secret, ev, chats, w.Description, w.Enabled, ms(now), ms(now))
	return err
}

const webhookCols = `id, owner, url, secret, events, chat_ids, description, enabled, created_at, updated_at`

func scanWebhook(sc interface{ Scan(...any) error }) (*Webhook, error) {
	var w Webhook
	var ev, chats string
	var created, updated int64
	if err := sc.Scan(&w.ID, &w.Owner, &w.URL, &w.Secret, &ev, &chats, &w.Description, &w.Enabled, &created, &updated); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(ev), &w.Events)
	json.Unmarshal([]byte(chats), &w.ChatIDs)
	w.CreatedAt, w.UpdatedAt = fromMS(created), fromMS(updated)
	return &w, nil
}

func (s *Store) ListWebhooks(ctx context.Context) ([]*Webhook, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+webhookCols+` FROM webhooks ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Webhook
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) GetWebhook(ctx context.Context, id string) (*Webhook, error) {
	w, err := scanWebhook(s.db.QueryRowContext(ctx, `SELECT `+webhookCols+` FROM webhooks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return w, err
}

// WebhookPatch holds optional updates (nil = unchanged).
type WebhookPatch struct {
	URL         *string
	Events      *[]string
	ChatIDs     *[]string
	Description *string
	Enabled     *bool
}

func (s *Store) UpdateWebhook(ctx context.Context, id string, p WebhookPatch) (*Webhook, error) {
	w, err := s.GetWebhook(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.URL != nil && *p.URL != w.URL {
		// A new destination gets a new secret, so repointing a webhook can't
		// redirect deliveries that the old receiver could verify.
		w.URL = *p.URL
		w.Secret = NewSecret()
	}
	if p.Events != nil {
		w.Events = *p.Events
	}
	if p.ChatIDs != nil {
		w.ChatIDs = *p.ChatIDs
	}
	if p.Description != nil {
		w.Description = *p.Description
	}
	if p.Enabled != nil {
		w.Enabled = *p.Enabled
	}
	now := s.now()
	w.UpdatedAt = now.UTC().Truncate(time.Millisecond)
	ev, _ := json.Marshal(w.Events)
	chats, _ := json.Marshal(w.ChatIDs)
	_, err = s.db.ExecContext(ctx, `UPDATE webhooks SET url = ?, secret = ?, events = ?, chat_ids = ?, description = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		w.URL, w.Secret, ev, chats, w.Description, w.Enabled, ms(now), id)
	return w, err
}

func (s *Store) DeleteWebhook(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- deliveries ----------------------------------------------------------------

type DeliveryStatus string

const (
	DeliveryPending   DeliveryStatus = "pending"
	DeliverySucceeded DeliveryStatus = "succeeded"
	DeliveryFailed    DeliveryStatus = "failed"
)

type Delivery struct {
	ID             string
	WebhookID      string
	EventID        string
	EventType      string
	Payload        []byte
	Status         DeliveryStatus
	Attempts       int
	NextAttemptAt  *time.Time
	LastStatusCode *int
	LastError      *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// EnqueueDelivery records a pending delivery due now.
func (s *Store) EnqueueDelivery(ctx context.Context, webhookID, eventID, eventType string, payload []byte) (*Delivery, error) {
	now := s.now()
	d := &Delivery{
		ID: randomID("dlv_", 8), WebhookID: webhookID, EventID: eventID, EventType: eventType, Payload: payload,
		Status: DeliveryPending, CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO webhook_deliveries
		(id, webhook_id, event_id, event_type, payload, status, attempts, next_attempt_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'pending', 0, ?, ?, ?)`,
		d.ID, webhookID, eventID, eventType, payload, ms(now), ms(now), ms(now))
	return d, err
}

const deliveryCols = `id, webhook_id, event_id, event_type, payload, status, attempts, next_attempt_at, last_status_code, last_error, created_at, updated_at`

func scanDelivery(sc interface{ Scan(...any) error }) (*Delivery, error) {
	var d Delivery
	var next, code sql.NullInt64
	var lastErr sql.NullString
	var created, updated int64
	if err := sc.Scan(&d.ID, &d.WebhookID, &d.EventID, &d.EventType, &d.Payload, &d.Status, &d.Attempts,
		&next, &code, &lastErr, &created, &updated); err != nil {
		return nil, err
	}
	if next.Valid {
		t := fromMS(next.Int64)
		d.NextAttemptAt = &t
	}
	if code.Valid {
		c := int(code.Int64)
		d.LastStatusCode = &c
	}
	if lastErr.Valid {
		d.LastError = &lastErr.String
	}
	d.CreatedAt, d.UpdatedAt = fromMS(created), fromMS(updated)
	return &d, nil
}

// DueDeliveries returns pending deliveries whose next attempt is due.
func (s *Store) DueDeliveries(ctx context.Context, limit int) ([]*Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries
		WHERE status = 'pending' AND next_attempt_at <= ? ORDER BY next_attempt_at, created_at LIMIT ?`, ms(s.now()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// NextDueAt returns when the earliest pending delivery is due (nil if none).
func (s *Store) NextDueAt(ctx context.Context) (*time.Time, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MIN(next_attempt_at) FROM webhook_deliveries WHERE status = 'pending'`).Scan(&v)
	if err != nil || !v.Valid {
		return nil, err
	}
	t := fromMS(v.Int64)
	return &t, nil
}

// RecordAttempt stores the result of a delivery attempt. next is the retry
// time when status is pending.
func (s *Store) RecordAttempt(ctx context.Context, id string, status DeliveryStatus, attempts int, next *time.Time, code *int, errMsg *string) error {
	var nextMS any
	if next != nil {
		nextMS = ms(*next)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE webhook_deliveries SET status = ?, attempts = ?, next_attempt_at = ?,
		last_status_code = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		status, attempts, nextMS, code, errMsg, ms(s.now()), id)
	return err
}

// ListDeliveries returns a webhook's most recent deliveries, newest first.
func (s *Store) ListDeliveries(ctx context.Context, webhookID string, limit int) ([]*Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deliveryCols+` FROM webhook_deliveries
		WHERE webhook_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`, webhookID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PruneDeliveries deletes finished deliveries older than before.
func (s *Store) PruneDeliveries(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE status != 'pending' AND updated_at < ?`, ms(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
