// Package fixture builds synthetic chat.db files for tests, using the real
// schema captured from macOS 26.5 (no private data).
package fixture

import (
	"database/sql"
	_ "embed"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/matthewmgamble/imsgd/internal/imessage/appledate"
	"github.com/matthewmgamble/imsgd/internal/imessage/typedstream"

	_ "modernc.org/sqlite"
)

//go:embed chat-macos-26.5.sql
var schemaSQL string

const (
	StyleGroup  = 43
	StyleDirect = 45
)

// Builder creates and populates a chat.db.
type Builder struct {
	t    testing.TB
	DB   *sql.DB
	Path string
	mu   sync.Mutex // guards seq; builders may be used from several goroutines
	seq  int
}

// New creates an empty chat.db with the macOS 26.5 schema in a temp dir.
// Triggers are dropped because they call Messages-internal SQL functions.
func New(t testing.TB) *Builder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.db")
	// WAL like the real chat.db, so readers and the writer don't block each other.
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("load schema: %v", err)
	}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'trigger'`)
	if err != nil {
		t.Fatal(err)
	}
	var triggers []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		triggers = append(triggers, n)
	}
	rows.Close()
	for _, n := range triggers {
		if _, err := db.Exec(`DROP TRIGGER "` + n + `"`); err != nil {
			t.Fatal(err)
		}
	}
	return &Builder{t: t, DB: db, Path: path}
}

func (b *Builder) exec(q string, args ...any) int64 {
	b.t.Helper()
	res, err := b.DB.Exec(q, args...)
	if err != nil {
		b.t.Fatalf("fixture: %v\n%s", err, q)
	}
	id, _ := res.LastInsertId()
	return id
}

func (b *Builder) guid(prefix string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	return fmt.Sprintf("%s-%04d-0000-0000-000000000000", prefix, b.seq)
}

func (b *Builder) nextSeq() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	return b.seq
}

// Handle inserts a handle and returns its ROWID.
func (b *Builder) Handle(address, service string) int64 {
	return b.exec(`INSERT INTO handle (id, service, country, uncanonicalized_id) VALUES (?, ?, 'us', ?)`,
		address, service, address)
}

// Chat inserts a chat with participants (in order) and returns its ROWID.
func (b *Builder) Chat(guid string, style int, displayName string, handles ...int64) int64 {
	id := b.exec(`INSERT INTO chat (guid, style, chat_identifier, service_name, display_name, last_read_message_timestamp)
		VALUES (?, ?, ?, 'iMessage', ?, 0)`, guid, style, guid, displayName)
	for _, h := range handles {
		b.exec(`INSERT INTO chat_handle_join (chat_id, handle_id) VALUES (?, ?)`, id, h)
	}
	return id
}

// SetLastRead sets chat.last_read_message_timestamp.
func (b *Builder) SetLastRead(chat int64, t time.Time) {
	b.exec(`UPDATE chat SET last_read_message_timestamp = ? WHERE ROWID = ?`, appledate.FromTime(t), chat)
}

// Msg describes a message row. Zero values mean "unset".
type Msg struct {
	GUID   string
	Chat   int64 // 0 = not joined to a chat
	Handle int64
	FromMe bool

	Text string
	// Runs override the default single part-0 run.
	Runs []typedstream.Run
	// PlainText stores Text in the text column instead of attributedBody
	// (pre-Ventura style).
	PlainText bool
	Subject   string
	Service   string // default iMessage

	Date, Read, Delivered, Edited, Retracted time.Time
	IsSent, IsDelivered                      bool
	Error                                    int64

	ItemType, GroupActionType   int64
	GroupTitle                  string
	ShareStatus, ShareDirection int64
	OtherHandle                 int64

	AssociatedGUID  string
	AssociatedType  int64
	AssociatedEmoji string

	ThreadOriginatorGUID string
	PartCount            int64
	SummaryInfo          []byte
	BalloonBundleID      string
}

// Message inserts a message (and its chat join) and returns ROWID and GUID.
func (b *Builder) Message(m Msg) (int64, string) {
	b.t.Helper()
	if m.GUID == "" {
		m.GUID = b.guid("MSG")
	}
	if m.Service == "" {
		m.Service = "iMessage"
	}
	if m.Date.IsZero() {
		m.Date = time.Date(2026, 1, 1, 12, 0, b.nextSeq(), 0, time.UTC)
	}
	var text any
	var body []byte
	if m.Text != "" || len(m.Runs) > 0 {
		if m.PlainText {
			text = m.Text
		} else {
			runs := m.Runs
			if runs == nil {
				runs = []typedstream.Run{{
					Length:     len(utf16.Encode([]rune(m.Text))),
					Attributes: map[string]any{"__kIMMessagePartAttributeName": 0},
				}}
			}
			body = typedstream.EncodeAttributedString(m.Text, runs)
		}
	}
	date := appledate.FromTime(m.Date)
	ts := func(t time.Time) int64 {
		if t.IsZero() {
			return 0
		}
		return appledate.FromTime(t)
	}
	var partCount any
	if m.PartCount > 0 || !m.Retracted.IsZero() {
		partCount = m.PartCount
	}
	id := b.exec(`INSERT INTO message (guid, text, attributedBody, subject, service, handle_id, other_handle,
			is_from_me, is_sent, is_delivered, is_finished, error,
			date, date_read, date_delivered, date_edited, date_retracted,
			item_type, group_action_type, group_title, share_status, share_direction,
			associated_message_guid, associated_message_type, associated_message_emoji,
			thread_originator_guid, part_count, message_summary_info, balloon_bundle_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.GUID, text, nilIfEmpty(body), nilIfEmptyStr(m.Subject), m.Service, m.Handle, m.OtherHandle,
		b2i(m.FromMe), b2i(m.IsSent), b2i(m.IsDelivered), 1, m.Error,
		date, ts(m.Read), ts(m.Delivered), ts(m.Edited), ts(m.Retracted),
		m.ItemType, m.GroupActionType, nilIfEmptyStr(m.GroupTitle), m.ShareStatus, m.ShareDirection,
		nilIfEmptyStr(m.AssociatedGUID), m.AssociatedType, nilIfEmptyStr(m.AssociatedEmoji),
		nilIfEmptyStr(m.ThreadOriginatorGUID), partCount, nilIfEmpty(m.SummaryInfo), nilIfEmptyStr(m.BalloonBundleID))
	if m.Chat != 0 {
		b.exec(`INSERT INTO chat_message_join (chat_id, message_id, message_date) VALUES (?, ?, ?)`, m.Chat, id, date)
	}
	return id, m.GUID
}

// Att describes an attachment row.
type Att struct {
	GUID            string
	Filename        string // may start with "~/"
	MimeType        string
	UTI             string
	TransferName    string
	TotalBytes      int64
	TransferState   int64 // default 5 (complete)
	AttributionInfo []byte
}

// Attachment inserts an attachment joined to message msg and returns ROWID and GUID.
func (b *Builder) Attachment(msg int64, a Att) (int64, string) {
	if a.GUID == "" {
		a.GUID = b.guid("ATT")
	}
	if a.TransferState == 0 {
		a.TransferState = 5
	}
	id := b.exec(`INSERT INTO attachment (guid, original_guid, filename, uti, mime_type, transfer_name, total_bytes,
			transfer_state, created_date, attribution_info)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.GUID, a.GUID, a.Filename, nilIfEmptyStr(a.UTI), nilIfEmptyStr(a.MimeType), a.TransferName, a.TotalBytes,
		a.TransferState, appledate.FromTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), nilIfEmpty(a.AttributionInfo))
	b.exec(`INSERT INTO message_attachment_join (message_id, attachment_id) VALUES (?, ?)`, msg, id)
	b.exec(`UPDATE message SET cache_has_attachments = 1 WHERE ROWID = ?`, msg)
	return id, a.GUID
}

func b2i(v bool) int {
	if v {
		return 1
	}
	return 0
}

func nilIfEmpty(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func nilIfEmptyStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Fast disables fsync on the fixture database, for bulk inserts in load tests.
func (b *Builder) Fast() {
	if _, err := b.DB.Exec(`PRAGMA synchronous = OFF`); err != nil {
		b.t.Fatal(err)
	}
}
