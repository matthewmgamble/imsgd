package db

import (
	"context"
	"time"

	"github.com/matthewmgamble/imsgd/internal/imessage/appledate"
)

// MessageState is the mutable part of a message row, used to detect
// updates cheaply without decoding bodies. Dates are raw chat.db values.
type MessageState struct {
	RowID         int64
	Date          int64
	IsSent        bool
	IsDelivered   bool
	IsRead        bool
	Error         int64
	DateDelivered int64
	DateRead      int64
	DatePlayed    int64
	DateEdited    int64
	DateRetracted int64
	PartCount     int64
}

// StateOf extracts the state of a fully loaded message.
func StateOf(m *Message) MessageState {
	raw := func(t *time.Time) int64 {
		if t == nil {
			return 0
		}
		return appledate.FromTime(*t)
	}
	return MessageState{
		RowID: m.RowID, Date: raw(m.Date),
		IsSent: m.IsSent, IsDelivered: m.IsDelivered, IsRead: m.IsRead, Error: m.Error,
		DateDelivered: raw(m.DateDelivered), DateRead: raw(m.DateRead), DatePlayed: raw(m.DatePlayed),
		DateEdited: raw(m.DateEdited), DateRetracted: raw(m.DateRetracted), PartCount: m.PartCount,
	}
}

// RecentMessageStates returns the state of every message dated at or after
// since (uses the message.date index).
func (d *DB) RecentMessageStates(ctx context.Context, since time.Time) ([]MessageState, error) {
	l := &selectList{s: d.schema, table: "message", alias: "m"}
	l.exprs = append(l.exprs, "m.ROWID")
	for _, c := range []string{"date", "is_sent", "is_delivered", "is_read", "error", "date_delivered",
		"date_read", "date_played", "date_edited", "date_retracted", "part_count"} {
		l.add(c, kInt)
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT `+l.String()+` FROM message m WHERE m.date >= ?`, appledate.FromTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageState
	for rows.Next() {
		var s MessageState
		var sent, delivered, read int64
		if err := rows.Scan(&s.RowID, &s.Date, &sent, &delivered, &read, &s.Error, &s.DateDelivered,
			&s.DateRead, &s.DatePlayed, &s.DateEdited, &s.DateRetracted, &s.PartCount); err != nil {
			return nil, err
		}
		s.IsSent, s.IsDelivered, s.IsRead = sent != 0, delivered != 0, read != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// ChatReadStates returns each chat's raw last_read_message_timestamp by GUID.
func (d *DB) ChatReadStates(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	if !d.schema.has("chat", "last_read_message_timestamp") {
		return out, nil
	}
	rows, err := d.sql.QueryContext(ctx, `SELECT guid, IFNULL(last_read_message_timestamp, 0) FROM chat`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g string
		var ts int64
		if err := rows.Scan(&g, &ts); err != nil {
			return nil, err
		}
		out[g] = ts
	}
	return out, rows.Err()
}
