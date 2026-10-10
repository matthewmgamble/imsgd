package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ErrConflict is returned when a client_ref is already used by the API key.
var ErrConflict = errors.New("conflict")

// Send and item statuses.
const (
	SendPending   = "pending"
	SendSent      = "sent"
	SendDelivered = "delivered"
	SendFailed    = "failed"
)

// Send is one POST /api/v1/messages request: text and/or attachments to a
// chat or address. Each part becomes its own message (an item).
type Send struct {
	ID        string
	APIKey    string
	ClientRef string
	ChatID    string // target chat, or the chat the first message landed in
	Recipient string // target address when sending to an address
	Service   string
	Status    string
	Error     string
	Items     []SendItem
	CreatedAt time.Time
	UpdatedAt time.Time
}

type SendItem struct {
	Index        int        `json:"index"`
	Kind         string     `json:"kind"` // text or attachment
	Text         string     `json:"text,omitempty"`
	Filename     string     `json:"filename,omitempty"`
	Path         string     `json:"path,omitempty"` // staged file
	Status       string     `json:"status"`
	MessageID    string     `json:"message_id,omitempty"`
	MessageRowID int64      `json:"message_rowid,omitempty"`
	Error        string     `json:"error,omitempty"`
	SubmittedAt  *time.Time `json:"submitted_at,omitempty"` // AppleScript returned
}

// NewSendID returns a new send ID.
func NewSendID() string { return randomID("snd_", 8) }

// CreateSend inserts s (ID must be set). It returns ErrConflict if the
// client_ref is already used by the same API key.
func (st *Store) CreateSend(ctx context.Context, s *Send) error {
	now := st.now().UTC().Truncate(time.Millisecond)
	s.CreatedAt, s.UpdatedAt = now, now
	items, _ := json.Marshal(s.Items)
	var ref any
	if s.ClientRef != "" {
		ref = s.ClientRef
	}
	_, err := st.db.ExecContext(ctx, `INSERT INTO sends (id, api_key, client_ref, chat_id, recipient, service, status, error, items, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.APIKey, ref, s.ChatID, s.Recipient, s.Service, s.Status, s.Error, items, ms(now), ms(now))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrConflict
	}
	return err
}

// UpdateSend persists s's mutable fields.
func (st *Store) UpdateSend(ctx context.Context, s *Send) error {
	s.UpdatedAt = st.now().UTC().Truncate(time.Millisecond)
	items, _ := json.Marshal(s.Items)
	_, err := st.db.ExecContext(ctx, `UPDATE sends SET chat_id = ?, status = ?, error = ?, items = ?, updated_at = ? WHERE id = ?`,
		s.ChatID, s.Status, s.Error, items, ms(s.UpdatedAt), s.ID)
	return err
}

const sendCols = `id, api_key, IFNULL(client_ref, ''), chat_id, recipient, service, status, error, items, created_at, updated_at`

func scanSend(sc interface{ Scan(...any) error }) (*Send, error) {
	var s Send
	var items string
	var created, updated int64
	if err := sc.Scan(&s.ID, &s.APIKey, &s.ClientRef, &s.ChatID, &s.Recipient, &s.Service, &s.Status, &s.Error, &items, &created, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(items), &s.Items); err != nil {
		return nil, err
	}
	s.CreatedAt, s.UpdatedAt = fromMS(created), fromMS(updated)
	return &s, nil
}

func (st *Store) GetSend(ctx context.Context, id string) (*Send, error) {
	s, err := scanSend(st.db.QueryRowContext(ctx, `SELECT `+sendCols+` FROM sends WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

// GetSendByClientRef finds a send by its idempotency key.
func (st *Store) GetSendByClientRef(ctx context.Context, apiKey, ref string) (*Send, error) {
	s, err := scanSend(st.db.QueryRowContext(ctx, `SELECT `+sendCols+` FROM sends WHERE api_key = ? AND client_ref = ?`, apiKey, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

// SendsWithStatus lists sends in a status (used to recover after restart).
func (st *Store) SendsWithStatus(ctx context.Context, status string) ([]*Send, error) {
	rows, err := st.db.QueryContext(ctx, `SELECT `+sendCols+` FROM sends WHERE status = ? ORDER BY created_at`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Send
	for rows.Next() {
		s, err := scanSend(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
