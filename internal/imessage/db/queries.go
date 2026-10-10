package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/matthewmgamble/imsgd/internal/imessage/appledate"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

const (
	DefaultLimit = 100
	// MaxLimit is a safety cap; the API enforces its own (smaller) page size
	// and asks for one extra row to detect further pages.
	MaxLimit = 5000
)

func clampLimit(n int) int {
	if n <= 0 {
		return DefaultLimit
	}
	return min(n, MaxLimit)
}

// queries holds SQL fragments built once from the probed schema.
type queries struct {
	messageCols    string
	attachmentCols string
	chatCols       string
	handleCols     string
	// msgDate is the per-chat ordering column: chat_message_join.message_date
	// (indexed with chat_id) when present, else message.date.
	msgDate string
}

func buildQueries(s schema) *queries {
	m := &selectList{s: s, table: "message", alias: "m"}
	m.exprs = append(m.exprs, "m.ROWID", "m.guid")
	m.add("text", kStr)
	m.add("attributedBody", kBlob)
	m.add("subject", kStr)
	m.add("service", kStr)
	m.add("handle_id", kInt)
	m.add("other_handle", kInt)
	m.add("is_from_me", kInt)
	m.add("is_sent", kInt)
	m.add("is_delivered", kInt)
	m.add("is_read", kInt)
	m.add("is_finished", kInt)
	m.add("is_audio_message", kInt)
	m.add("is_system_message", kInt)
	m.add("error", kInt)
	m.add("date", kInt)
	m.add("date_read", kInt)
	m.add("date_delivered", kInt)
	m.add("date_played", kInt)
	m.add("date_edited", kInt)
	m.add("date_retracted", kInt)
	m.add("item_type", kInt)
	m.add("group_action_type", kInt)
	m.add("group_title", kStr)
	m.add("share_status", kInt)
	m.add("share_direction", kInt)
	m.add("associated_message_guid", kStr)
	m.add("associated_message_type", kInt)
	m.add("associated_message_emoji", kStr)
	m.add("balloon_bundle_id", kStr)
	m.add("expressive_send_style_id", kStr)
	m.add("thread_originator_guid", kStr)
	m.add("thread_originator_part", kStr)
	m.add("reply_to_guid", kStr)
	m.add("part_count", kInt)
	m.add("message_summary_info", kBlob)
	m.add("cache_has_attachments", kInt)
	m.add("schedule_type", kInt)
	m.exprs = append(m.exprs, "IFNULL(cmj.chat_id, 0)", "IFNULL(c.guid, '')")

	a := &selectList{s: s, table: "attachment", alias: "a"}
	a.exprs = append(a.exprs, "a.ROWID", "a.guid")
	a.add("filename", kStr)
	a.add("uti", kStr)
	a.add("mime_type", kStr)
	a.add("transfer_name", kStr)
	a.add("total_bytes", kInt)
	a.add("transfer_state", kInt)
	a.add("is_outgoing", kInt)
	a.add("is_sticker", kInt)
	a.add("hide_attachment", kInt)
	a.add("created_date", kInt)
	a.add("attribution_info", kBlob)

	c := &selectList{s: s, table: "chat", alias: "c"}
	c.exprs = append(c.exprs, "c.ROWID", "c.guid")
	c.add("style", kInt)
	c.add("chat_identifier", kStr)
	c.add("service_name", kStr)
	c.add("display_name", kStr)
	c.add("group_id", kStr)
	c.add("is_archived", kInt)
	c.add("is_filtered", kInt)
	c.add("last_read_message_timestamp", kInt)

	h := &selectList{s: s, table: "handle", alias: "h"}
	h.exprs = append(h.exprs, "h.ROWID", "h.id")
	h.add("service", kStr)
	h.add("country", kStr)
	h.add("uncanonicalized_id", kStr)

	msgDate := "m.date"
	if s.has("chat_message_join", "message_date") {
		msgDate = "cmj.message_date"
	}
	return &queries{
		messageCols:    m.String(),
		attachmentCols: a.String(),
		chatCols:       c.String(),
		handleCols:     h.String(),
		msgDate:        msgDate,
	}
}

const (
	// fromMessages starts from message; chat columns are joined when present.
	fromMessages = ` FROM message m
		LEFT JOIN chat_message_join cmj ON cmj.message_id = m.ROWID
		LEFT JOIN chat c ON c.ROWID = cmj.chat_id`
	// fromChatMessages starts from the join table to use its
	// (chat_id, message_date, message_id) index.
	fromChatMessages = ` FROM chat_message_join cmj
		JOIN message m ON m.ROWID = cmj.message_id
		JOIN chat c ON c.ROWID = cmj.chat_id`
)

// ---- messages ---------------------------------------------------------------

func (d *DB) queryMessages(ctx context.Context, query string, args ...any) ([]*Message, error) {
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Message
	seen := map[int64]bool{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		if seen[m.RowID] { // a message joined to several chats
			continue
		}
		seen[m.RowID] = true
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := d.loadAttachments(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func scanMessage(rows *sql.Rows) (*Message, error) {
	var (
		m                                         Message
		text                                      string
		attributed, summary                       []byte
		fromMe, sent, delivered, read, finished   int64
		audio, system, hasAtt                     int64
		date, dRead, dDelivered, dPlayed, dEdited int64
		dRetracted                                int64
	)
	err := rows.Scan(
		&m.RowID, &m.GUID, &text, &attributed, &m.Subject, &m.Service,
		&m.HandleRowID, &m.OtherHandle,
		&fromMe, &sent, &delivered, &read, &finished, &audio, &system, &m.Error,
		&date, &dRead, &dDelivered, &dPlayed, &dEdited, &dRetracted,
		&m.ItemType, &m.GroupActionType, &m.GroupTitle, &m.ShareStatus, &m.ShareDirection,
		&m.AssociatedGUID, &m.AssociatedType, &m.AssociatedEmoji,
		&m.BalloonBundleID, &m.ExpressiveSendStyleID,
		&m.ThreadOriginatorGUID, &m.ThreadOriginatorPart, &m.ReplyToGUID,
		&m.PartCount, &summary, &hasAtt, &m.ScheduleType,
		&m.ChatRowID, &m.ChatGUID,
	)
	if err != nil {
		return nil, err
	}
	m.Body = decodeBody(attributed, text)
	m.IsFromMe = fromMe != 0
	m.IsSent = sent != 0
	m.IsDelivered = delivered != 0
	m.IsRead = read != 0
	m.IsFinished = finished != 0
	m.IsAudioMessage = audio != 0
	m.IsSystemMessage = system != 0
	m.HasAttachments = hasAtt != 0
	m.Date = appledate.Ptr(date)
	m.DateRead = appledate.Ptr(dRead)
	m.DateDelivered = appledate.Ptr(dDelivered)
	m.DatePlayed = appledate.Ptr(dPlayed)
	m.DateEdited = appledate.Ptr(dEdited)
	m.DateRetracted = appledate.Ptr(dRetracted)
	if len(summary) > 0 {
		si := decodeSummaryInfo(summary)
		m.EditedParts, m.RetractedParts, m.Edits = si.editedParts, si.retractedParts, si.edits
	}
	return &m, nil
}

// MessagesSince returns messages with ROWID > since in ascending ROWID
// order, optionally limited to one chat. ROWIDs only grow, so this is the
// primitive for incremental sync.
func (d *DB) MessagesSince(ctx context.Context, since int64, chatGUID string, limit int) ([]*Message, error) {
	q := `SELECT ` + d.q.messageCols + fromMessages + ` WHERE m.ROWID > ?`
	args := []any{since}
	if chatGUID != "" {
		q += ` AND c.guid = ?`
		args = append(args, chatGUID)
	}
	q += ` ORDER BY m.ROWID ASC LIMIT ?`
	args = append(args, clampLimit(limit))
	return d.queryMessages(ctx, q, args...)
}

// MessagesByRowID fetches specific messages (any order).
func (d *DB) MessagesByRowID(ctx context.Context, ids []int64) ([]*Message, error) {
	var out []*Message
	for _, chunk := range chunks(ids, 500) {
		q := `SELECT ` + d.q.messageCols + fromMessages + ` WHERE m.ROWID IN (` + placeholders(len(chunk)) + `)`
		ms, err := d.queryMessages(ctx, q, int64Args(chunk)...)
		if err != nil {
			return nil, err
		}
		out = append(out, ms...)
	}
	return out, nil
}

// GetMessage returns a message by GUID.
func (d *DB) GetMessage(ctx context.Context, guid string) (*Message, error) {
	ms, err := d.queryMessages(ctx, `SELECT `+d.q.messageCols+fromMessages+` WHERE m.guid = ?`, guid)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, ErrNotFound
	}
	return ms[0], nil
}

// Cursor is a keyset position within a chat's timeline. Date is the raw
// chat.db date value.
type Cursor struct {
	Date  int64
	RowID int64
}

// CursorFor returns the cursor positioned at m.
func CursorFor(m *Message) Cursor {
	c := Cursor{RowID: m.RowID}
	if m.Date != nil {
		c.Date = appledate.FromTime(*m.Date)
	}
	return c
}

// ChatMessagesQuery pages through one chat's messages.
type ChatMessagesQuery struct {
	ChatGUID string
	Before   *Cursor // exclusive upper bound
	After    *Cursor // exclusive lower bound
	Limit    int
	// Ascending returns oldest first; default is newest first.
	Ascending bool
}

// ChatMessages returns messages in a chat ordered by date (then ROWID).
func (d *DB) ChatMessages(ctx context.Context, q ChatMessagesQuery) ([]*Message, error) {
	var chatID int64
	err := d.sql.QueryRowContext(ctx, `SELECT ROWID FROM chat WHERE guid = ?`, q.ChatGUID).Scan(&chatID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	date := d.q.msgDate
	sqlq := `SELECT ` + d.q.messageCols + fromChatMessages + ` WHERE cmj.chat_id = ?`
	args := []any{chatID}
	if q.Before != nil {
		sqlq += fmt.Sprintf(` AND (%s, cmj.message_id) < (?, ?)`, date)
		args = append(args, q.Before.Date, q.Before.RowID)
	}
	if q.After != nil {
		sqlq += fmt.Sprintf(` AND (%s, cmj.message_id) > (?, ?)`, date)
		args = append(args, q.After.Date, q.After.RowID)
	}
	dir := "DESC"
	if q.Ascending {
		dir = "ASC"
	}
	sqlq += fmt.Sprintf(` ORDER BY %s %s, cmj.message_id %s LIMIT ?`, date, dir, dir)
	args = append(args, clampLimit(q.Limit))
	return d.queryMessages(ctx, sqlq, args...)
}

// LastMessages returns the newest message of each given chat.
func (d *DB) LastMessages(ctx context.Context, chatRowIDs []int64) (map[int64]*Message, error) {
	out := make(map[int64]*Message, len(chatRowIDs))
	q := `SELECT ` + d.q.messageCols + fromChatMessages +
		` WHERE cmj.chat_id = ? ORDER BY ` + d.q.msgDate + ` DESC, cmj.message_id DESC LIMIT 1`
	for _, id := range chatRowIDs {
		ms, err := d.queryMessages(ctx, q, id)
		if err != nil {
			return nil, err
		}
		if len(ms) == 1 {
			out[id] = ms[0]
		}
	}
	return out, nil
}

// MessageGUID returns the GUID of the message with the given ROWID.
func (d *DB) MessageGUID(ctx context.Context, rowID int64) (string, error) {
	var g string
	err := d.sql.QueryRowContext(ctx, `SELECT guid FROM message WHERE ROWID = ?`, rowID).Scan(&g)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return g, err
}

// MaxMessageRowID returns the highest message ROWID (0 if empty).
func (d *DB) MaxMessageRowID(ctx context.Context) (int64, error) {
	var n int64
	err := d.sql.QueryRowContext(ctx, `SELECT IFNULL(MAX(ROWID), 0) FROM message`).Scan(&n)
	return n, err
}

// ---- attachments -----------------------------------------------------------

func (d *DB) loadAttachments(ctx context.Context, msgs []*Message) error {
	var ids []int64
	byID := map[int64]*Message{}
	for _, m := range msgs {
		// cache_has_attachments is not always reliable for sticker/app
		// messages, so check every message.
		ids = append(ids, m.RowID)
		byID[m.RowID] = m
	}
	for _, chunk := range chunks(ids, 500) {
		q := `SELECT maj.message_id, ` + d.q.attachmentCols + `
			FROM message_attachment_join maj JOIN attachment a ON a.ROWID = maj.attachment_id
			WHERE maj.message_id IN (` + placeholders(len(chunk)) + `)
			ORDER BY maj.message_id, a.ROWID`
		rows, err := d.sql.QueryContext(ctx, q, int64Args(chunk)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var msgID int64
			a, err := scanAttachment(rows, &msgID)
			if err != nil {
				rows.Close()
				return err
			}
			a.MessageRowID = msgID
			m := byID[msgID]
			m.Attachments = append(m.Attachments, *a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

func scanAttachment(rows *sql.Rows, prefix ...any) (*Attachment, error) {
	var (
		a                       Attachment
		outgoing, sticker, hide int64
		created                 int64
		attribution             []byte
	)
	dest := append(prefix,
		&a.RowID, &a.GUID, &a.Filename, &a.UTI, &a.MimeType, &a.TransferName,
		&a.TotalBytes, &a.TransferState, &outgoing, &sticker, &hide, &created, &attribution)
	if err := rows.Scan(dest...); err != nil {
		return nil, err
	}
	a.IsOutgoing = outgoing != 0
	a.IsSticker = sticker != 0
	a.HideAttachment = hide != 0
	a.CreatedAt = appledate.Ptr(created)
	a.Filename = expandHome(a.Filename)
	a.PreviewWidth, a.PreviewHeight = decodeDimensions(attribution)
	if a.MimeType == "" {
		a.MimeType = guessMime(a.TransferName, a.Filename)
	}
	return &a, nil
}

// GetAttachment returns an attachment by GUID, with MessageRowID set to the
// first message referencing it.
func (d *DB) GetAttachment(ctx context.Context, guid string) (*Attachment, error) {
	q := `SELECT IFNULL((SELECT MIN(message_id) FROM message_attachment_join WHERE attachment_id = a.ROWID), 0), ` +
		d.q.attachmentCols + ` FROM attachment a WHERE a.guid = ?`
	rows, err := d.sql.QueryContext(ctx, q, guid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, ErrNotFound
	}
	var msgID int64
	a, err := scanAttachment(rows, &msgID)
	if err != nil {
		return nil, err
	}
	a.MessageRowID = msgID
	return a, nil
}

func guessMime(names ...string) string {
	for _, n := range names {
		if ext := filepath.Ext(n); ext != "" {
			if t := mime.TypeByExtension(strings.ToLower(ext)); t != "" {
				return t
			}
		}
	}
	return "application/octet-stream"
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

// ---- chats -----------------------------------------------------------------

// ChatCursor is a keyset position in the chat list (ordered by last activity).
type ChatCursor struct {
	LastDate int64
	RowID    int64
}

type ChatsQuery struct {
	Before *ChatCursor
	Limit  int
	// Search matches display name, chat identifier or a participant address.
	Search              string
	IncludeParticipants bool
}

func (d *DB) chatSelect() string {
	return `SELECT ` + d.q.chatCols + `,
		IFNULL((SELECT MAX(message_date) FROM chat_message_join WHERE chat_id = c.ROWID), 0) AS last_date
		FROM chat c`
}

// ListChats returns chats ordered by most recent activity.
func (d *DB) ListChats(ctx context.Context, q ChatsQuery) ([]*Chat, error) {
	inner := d.chatSelect()
	var conds []string
	var args []any
	if q.Search != "" {
		like := "%" + escapeLike(q.Search) + "%"
		conds = append(conds, `(c.display_name LIKE ? ESCAPE '\' OR c.chat_identifier LIKE ? ESCAPE '\'
			OR EXISTS (SELECT 1 FROM chat_handle_join chj JOIN handle h ON h.ROWID = chj.handle_id
			           WHERE chj.chat_id = c.ROWID AND h.id LIKE ? ESCAPE '\'))`)
		args = append(args, like, like, like)
	}
	if len(conds) > 0 {
		inner += ` WHERE ` + strings.Join(conds, " AND ")
	}
	sqlq := `SELECT * FROM (` + inner + `) WHERE 1=1`
	if q.Before != nil {
		sqlq += ` AND (last_date, ROWID) < (?, ?)`
		args = append(args, q.Before.LastDate, q.Before.RowID)
	}
	sqlq += ` ORDER BY last_date DESC, ROWID DESC LIMIT ?`
	args = append(args, clampLimit(q.Limit))

	chats, err := d.queryChats(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	if q.IncludeParticipants {
		if err := d.loadParticipants(ctx, chats); err != nil {
			return nil, err
		}
	}
	return chats, nil
}

// GetChat returns a chat by GUID, including participants.
func (d *DB) GetChat(ctx context.Context, guid string) (*Chat, error) {
	chats, err := d.queryChats(ctx, d.chatSelect()+` WHERE c.guid = ?`, guid)
	if err != nil {
		return nil, err
	}
	if len(chats) == 0 {
		return nil, ErrNotFound
	}
	if err := d.loadParticipants(ctx, chats); err != nil {
		return nil, err
	}
	return chats[0], nil
}

// ChatsByRowID returns chats (with participants) keyed by ROWID.
func (d *DB) ChatsByRowID(ctx context.Context, ids []int64) (map[int64]*Chat, error) {
	out := map[int64]*Chat{}
	for _, chunk := range chunks(ids, 500) {
		chats, err := d.queryChats(ctx, d.chatSelect()+` WHERE c.ROWID IN (`+placeholders(len(chunk))+`)`, int64Args(chunk)...)
		if err != nil {
			return nil, err
		}
		if err := d.loadParticipants(ctx, chats); err != nil {
			return nil, err
		}
		for _, c := range chats {
			out[c.RowID] = c
		}
	}
	return out, nil
}

func (d *DB) queryChats(ctx context.Context, q string, args ...any) ([]*Chat, error) {
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Chat
	for rows.Next() {
		var c Chat
		var archived, filtered, lastRead, lastDate int64
		if err := rows.Scan(&c.RowID, &c.GUID, &c.Style, &c.ChatIdentifier, &c.ServiceName,
			&c.DisplayName, &c.GroupID, &archived, &filtered, &lastRead, &lastDate); err != nil {
			return nil, err
		}
		c.IsArchived = archived != 0
		c.IsFiltered = filtered != 0
		c.LastReadAt = appledate.Ptr(lastRead)
		c.LastMessageAt = appledate.Ptr(lastDate)
		out = append(out, &c)
	}
	return out, rows.Err()
}

// ChatCursorFor returns the list cursor positioned at c.
func ChatCursorFor(c *Chat) ChatCursor {
	cur := ChatCursor{RowID: c.RowID}
	if c.LastMessageAt != nil {
		cur.LastDate = appledate.FromTime(*c.LastMessageAt)
	}
	return cur
}

func (d *DB) loadParticipants(ctx context.Context, chats []*Chat) error {
	if len(chats) == 0 {
		return nil
	}
	byID := map[int64]*Chat{}
	ids := make([]int64, 0, len(chats))
	for _, c := range chats {
		byID[c.RowID] = c
		ids = append(ids, c.RowID)
	}
	for _, chunk := range chunks(ids, 500) {
		// chat_handle_join has no ordering column; its rowid preserves
		// insertion order, which is the participant order Messages shows.
		q := `SELECT chj.chat_id, ` + d.q.handleCols + `
			FROM chat_handle_join chj JOIN handle h ON h.ROWID = chj.handle_id
			WHERE chj.chat_id IN (` + placeholders(len(chunk)) + `)
			ORDER BY chj.chat_id, chj.rowid`
		rows, err := d.sql.QueryContext(ctx, q, int64Args(chunk)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var chatID int64
			var h Handle
			if err := rows.Scan(&chatID, &h.RowID, &h.Address, &h.Service, &h.Country, &h.Uncanonicalized); err != nil {
				rows.Close()
				return err
			}
			byID[chatID].Participants = append(byID[chatID].Participants, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// ---- handles ---------------------------------------------------------------

type HandlesQuery struct {
	Search     string
	AfterRowID int64
	Limit      int
}

// ListHandles returns handles in ROWID order.
func (d *DB) ListHandles(ctx context.Context, q HandlesQuery) ([]Handle, error) {
	sqlq := `SELECT ` + d.q.handleCols + ` FROM handle h WHERE h.ROWID > ?`
	args := []any{q.AfterRowID}
	if q.Search != "" {
		sqlq += ` AND (h.id LIKE ? ESCAPE '\' OR h.uncanonicalized_id LIKE ? ESCAPE '\')`
		like := "%" + escapeLike(q.Search) + "%"
		args = append(args, like, like)
	}
	sqlq += ` ORDER BY h.ROWID LIMIT ?`
	args = append(args, clampLimit(q.Limit))
	return d.queryHandles(ctx, sqlq, args...)
}

// HandlesByRowID returns handles keyed by ROWID.
func (d *DB) HandlesByRowID(ctx context.Context, ids []int64) (map[int64]Handle, error) {
	out := map[int64]Handle{}
	for _, chunk := range chunks(ids, 500) {
		hs, err := d.queryHandles(ctx, `SELECT `+d.q.handleCols+` FROM handle h WHERE h.ROWID IN (`+placeholders(len(chunk))+`)`, int64Args(chunk)...)
		if err != nil {
			return nil, err
		}
		for _, h := range hs {
			out[h.RowID] = h
		}
	}
	return out, nil
}

func (d *DB) queryHandles(ctx context.Context, q string, args ...any) ([]Handle, error) {
	rows, err := d.sql.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Handle
	for rows.Next() {
		var h Handle
		if err := rows.Scan(&h.RowID, &h.Address, &h.Service, &h.Country, &h.Uncanonicalized); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ---- helpers ---------------------------------------------------------------

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func chunks(ids []int64, size int) [][]int64 {
	var out [][]int64
	for len(ids) > 0 {
		n := min(size, len(ids))
		out = append(out, ids[:n])
		ids = ids[n:]
	}
	return out
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
