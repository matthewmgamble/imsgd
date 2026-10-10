// Package db provides read-only access to the Messages database (chat.db).
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

// DB is a read-only handle to chat.db.
type DB struct {
	sql    *sql.DB
	path   string
	schema schema
	q      *queries
}

// Open opens chat.db read-only. The WAL remains visible (we deliberately do
// not use immutable=1), so new messages appear without reopening.
func Open(path string) (*DB, error) {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "query_only(1)")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()

	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	sqldb.SetMaxOpenConns(4)
	if err := sqldb.Ping(); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	d := &DB{sql: sqldb, path: path}
	d.schema, err = probeSchema(context.Background(), d,
		"message", "chat", "handle", "attachment", "chat_message_join", "chat_handle_join", "message_attachment_join")
	if err != nil {
		sqldb.Close()
		return nil, err
	}
	d.q = buildQueries(d.schema)
	return d, nil
}

func (d *DB) Close() error { return d.sql.Close() }

// Path returns the database file path.
func (d *DB) Path() string { return d.path }

// SQL exposes the underlying handle for packages in this module.
func (d *DB) SQL() *sql.DB { return d.sql }

// Stats is a quick summary used by `imsgd doctor`.
type Stats struct {
	Messages int64
	Chats    int64
	Handles  int64
	MaxRowID int64
}

func (d *DB) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := d.sql.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM message),
			(SELECT COUNT(*) FROM chat),
			(SELECT COUNT(*) FROM handle),
			(SELECT IFNULL(MAX(ROWID), 0) FROM message)`).
		Scan(&s.Messages, &s.Chats, &s.Handles, &s.MaxRowID)
	return s, err
}
