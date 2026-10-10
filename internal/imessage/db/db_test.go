package db_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"howett.net/plist"

	"github.com/matthewmgamble/imsgd/internal/imessage/db"
	"github.com/matthewmgamble/imsgd/internal/imessage/fixture"
	"github.com/matthewmgamble/imsgd/internal/imessage/typedstream"
)

var ctx = context.Background()

func at(min int) time.Time { return time.Date(2026, 3, 1, 10, min, 0, 0, time.UTC) }

// world is a small but representative message database.
type world struct {
	f                    *fixture.Builder
	alice, bob, carol    int64
	dm, group, quiet     int64
	dmGUID, groupGUID    string
	hello, reply, photo  string // message GUIDs
	edited, unsent, love string
	unsent26             string
}

func buildWorld(t *testing.T) *world {
	f := fixture.New(t)
	w := &world{f: f, dmGUID: "any;-;+15550000001", groupGUID: "any;+;chat0001"}
	w.carol = f.Handle("carol@example.com", "iMessage")
	w.alice = f.Handle("+15550000001", "iMessage")
	w.bob = f.Handle("+15550000002", "iMessage")

	w.dm = f.Chat(w.dmGUID, fixture.StyleDirect, "", w.alice)
	// Participant order follows insertion, not handle ROWID.
	w.group = f.Chat(w.groupGUID, fixture.StyleGroup, "Climbing", w.bob, w.alice, w.carol)
	w.quiet = f.Chat("any;-;quiet@example.com", fixture.StyleDirect, "", w.carol)

	_, w.hello = f.Message(fixture.Msg{Chat: w.dm, Handle: w.alice, Text: "hello there", Date: at(1), IsDelivered: true})
	_, w.reply = f.Message(fixture.Msg{Chat: w.dm, Handle: w.alice, FromMe: true, Text: "hi!", Date: at(2),
		IsSent: true, IsDelivered: true, Delivered: at(2), Read: at(3)})
	// A tapback on part 0 of "hello there".
	_, w.love = f.Message(fixture.Msg{Chat: w.dm, Handle: w.alice, FromMe: true, Text: "Loved “hello there”", Date: at(4),
		AssociatedGUID: "p:0/" + w.hello, AssociatedType: 2000})

	// Group: text + attachment in one message, with a mention.
	text := "@Bob look ￼"
	photoID, photo := f.Message(fixture.Msg{Chat: w.group, Handle: w.carol, Text: text, Date: at(5), Runs: []typedstream.Run{
		{Length: 4, Attributes: map[string]any{"__kIMMessagePartAttributeName": 0, "__kIMMentionConfirmedMention": "+15550000002"}},
		{Length: 6, Attributes: map[string]any{"__kIMMessagePartAttributeName": 0}},
		{Length: 1, Attributes: map[string]any{"__kIMMessagePartAttributeName": 1, "__kIMFileTransferGUIDAttributeName": "ATT-PHOTO"}},
	}})
	w.photo = photo
	attribution, _ := plist.Marshal(map[string]any{"pgensw": float32(4032), "pgensh": float32(3024)}, plist.BinaryFormat)
	f.Attachment(photoID, fixture.Att{GUID: "ATT-PHOTO", Filename: "~/Library/Messages/Attachments/ab/IMG_1.heic",
		MimeType: "image/heic", UTI: "public.heic", TransferName: "IMG_1.heic", TotalBytes: 1234, AttributionInfo: attribution})

	// Group events.
	f.Message(fixture.Msg{Chat: w.group, Handle: w.bob, ItemType: 1, GroupActionType: 0, OtherHandle: w.carol, Date: at(6)})
	f.Message(fixture.Msg{Chat: w.group, Handle: w.bob, ItemType: 2, GroupTitle: "Climbing", Date: at(7)})
	f.Message(fixture.Msg{Chat: w.group, Handle: w.alice, ItemType: 3, GroupActionType: 0, Date: at(8)})
	// Emoji tapback and a removed "like".
	f.Message(fixture.Msg{Chat: w.group, Handle: w.bob, Text: "Reacted 🔥", Date: at(9),
		AssociatedGUID: "p:1/" + photo, AssociatedType: 2006, AssociatedEmoji: "🔥"})
	f.Message(fixture.Msg{Chat: w.group, Handle: w.alice, Date: at(10), AssociatedGUID: "bp:" + photo, AssociatedType: 3001})

	// An edited message with history, and a fully unsent one.
	oldBody := typedstream.EncodeAttributedString("helo", nil)
	summary, _ := plist.Marshal(map[string]any{
		"ep": []any{0},
		"ec": map[string]any{"0": []any{map[string]any{"d": 8.1e8, "t": oldBody}}},
	}, plist.BinaryFormat)
	_, w.edited = f.Message(fixture.Msg{Chat: w.dm, Handle: w.alice, FromMe: true, Text: "hello", Date: at(11),
		Edited: at(12), SummaryInfo: summary, PartCount: 1})
	_, w.unsent = f.Message(fixture.Msg{Chat: w.dm, Handle: w.alice, FromMe: true, Date: at(13), Retracted: at(14), PartCount: 0})

	// macOS 26 style unsend: date_edited + "rp" + part_count 0, no date_retracted.
	rpSummary, _ := plist.Marshal(map[string]any{"rp": []any{0}, "ep": []any{0}}, plist.BinaryFormat)
	_, w.unsent26 = f.Message(fixture.Msg{Chat: w.quiet, Handle: w.carol, Date: at(0).Add(-time.Minute),
		Edited: at(0), SummaryInfo: rpSummary, PartCount: 0})
	// Location sharing notice.
	f.Message(fixture.Msg{Chat: w.quiet, Handle: w.carol, ItemType: 4, ShareDirection: 1, Date: at(0).Add(-2 * time.Minute)})

	// Pre-Ventura style plain text, and a message not joined to any chat.
	f.Message(fixture.Msg{Chat: w.quiet, Handle: w.carol, Text: "plain text column", PlainText: true, Date: at(0)})
	f.Message(fixture.Msg{Handle: w.carol, Text: "orphan", Date: at(15)})
	f.SetLastRead(w.dm, at(3))
	return w
}

func open(t *testing.T, w *world) *db.DB {
	d, err := db.Open(w.f.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func guids(ms []*db.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.GUID
	}
	return out
}

func TestMessagesSince(t *testing.T) {
	w := buildWorld(t)
	d := open(t, w)

	all, err := d.MessagesSince(ctx, 0, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 15 {
		t.Fatalf("got %d messages, want 15", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].RowID <= all[i-1].RowID {
			t.Fatal("not in ROWID order")
		}
	}
	// Paging by ROWID.
	page, _ := d.MessagesSince(ctx, all[4].RowID, "", 3)
	if len(page) != 3 || page[0].RowID != all[5].RowID {
		t.Fatalf("page = %v", guids(page))
	}
	// Chat filter.
	dm, _ := d.MessagesSince(ctx, 0, w.dmGUID, 0)
	if want := []string{w.hello, w.reply, w.love, w.edited, w.unsent}; !slices.Equal(guids(dm), want) {
		t.Fatalf("dm = %v, want %v", guids(dm), want)
	}
	// Orphan message has no chat.
	last := all[len(all)-1]
	if last.ChatGUID != "" || last.Text() != "orphan" {
		t.Fatalf("orphan = %+v", last)
	}
}

func TestMessageFields(t *testing.T) {
	w := buildWorld(t)
	d := open(t, w)

	m, err := d.GetMessage(ctx, w.reply)
	if err != nil {
		t.Fatal(err)
	}
	if m.Text() != "hi!" || !m.IsFromMe || !m.IsSent || !m.IsDelivered || m.ChatGUID != w.dmGUID {
		t.Fatalf("reply = %+v", m)
	}
	if !m.Date.Equal(at(2)) || !m.DateRead.Equal(at(3)) || m.DateEdited != nil {
		t.Fatalf("dates: %v %v %v", m.Date, m.DateRead, m.DateEdited)
	}

	if _, err := d.GetMessage(ctx, "nope"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestRichBody(t *testing.T) {
	w := buildWorld(t)
	d := open(t, w)
	m, err := d.GetMessage(ctx, w.photo)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Text(); got != "@Bob look" {
		t.Fatalf("text = %q", got)
	}
	b := m.Body
	if len(b.Parts) != 2 || b.Parts[0].Text != "@Bob look" || b.Parts[1].AttachmentGUID != "ATT-PHOTO" || b.Parts[1].Index != 1 {
		t.Fatalf("parts = %+v", b.Parts)
	}
	if len(b.Mentions) != 1 || b.Mentions[0].Address != "+15550000002" || b.Mentions[0].Start != 0 || b.Mentions[0].Length != 4 {
		t.Fatalf("mentions = %+v", b.Mentions)
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("attachments = %+v", m.Attachments)
	}
	a := m.Attachments[0]
	home, _ := os.UserHomeDir()
	if a.Filename != filepath.Join(home, "Library/Messages/Attachments/ab/IMG_1.heic") {
		t.Fatalf("filename not expanded: %s", a.Filename)
	}
	if a.PreviewWidth != 4032 || a.PreviewHeight != 3024 || a.MimeType != "image/heic" {
		t.Fatalf("attachment = %+v", a)
	}
}

func TestPlainTextColumn(t *testing.T) {
	w := buildWorld(t)
	d := open(t, w)
	ms, _ := d.MessagesSince(ctx, 0, "any;-;quiet@example.com", 0)
	if len(ms) != 3 || ms[2].Text() != "plain text column" {
		t.Fatalf("got %+v", ms)
	}
}

func TestReactions(t *testing.T) {
	w := buildWorld(t)
	d := open(t, w)

	m, _ := d.GetMessage(ctx, w.love)
	r := m.Reaction()
	if r == nil || r.Kind != db.ReactionLove || r.Removed || r.TargetGUID != w.hello || r.TargetPart != 0 {
		t.Fatalf("love = %+v", r)
	}

	group, _ := d.MessagesSince(ctx, 0, w.groupGUID, 0)
	var emoji, removed *db.Reaction
	for _, m := range group {
		if r := m.Reaction(); r != nil {
			if r.Kind == db.ReactionEmoji {
				emoji = r
			} else {
				removed = r
			}
		}
	}
	if emoji == nil || emoji.Emoji != "🔥" || emoji.TargetPart != 1 || emoji.TargetGUID != w.photo {
		t.Fatalf("emoji = %+v", emoji)
	}
	if removed == nil || removed.Kind != db.ReactionLike || !removed.Removed || removed.TargetGUID != w.photo {
		t.Fatalf("removed = %+v", removed)
	}

	plain, _ := d.GetMessage(ctx, w.hello)
	if plain.Reaction() != nil {
		t.Fatal("ordinary message reported as reaction")
	}
}

func TestGroupEvents(t *testing.T) {
	w := buildWorld(t)
	d := open(t, w)
	ms, _ := d.MessagesSince(ctx, 0, w.groupGUID, 0)
	var kinds []db.GroupEventKind
	for _, m := range ms {
		if ev := m.GroupEvent(); ev != nil {
			kinds = append(kinds, ev.Kind)
			switch ev.Kind {
			case db.GroupParticipantAdded:
				if ev.ParticipantRowID != w.carol {
					t.Errorf("added participant = %d", ev.ParticipantRowID)
				}
			case db.GroupRenamed:
				if ev.Title != "Climbing" {
					t.Errorf("title = %q", ev.Title)
				}
			case db.GroupParticipantLeft:
				if ev.ParticipantRowID != w.alice {
					t.Errorf("left participant = %d", ev.ParticipantRowID)
				}
			}
		}
	}
	want := []db.GroupEventKind{db.GroupParticipantAdded, db.GroupRenamed, db.GroupParticipantLeft}
	if !slices.Equal(kinds, want) {
		t.Fatalf("events = %v", kinds)
	}
}

func TestEditsAndUnsend(t *testing.T) {
	w := buildWorld(t)
	d := open(t, w)

	m, _ := d.GetMessage(ctx, w.edited)
	if m.DateEdited == nil || !slices.Equal(m.EditedParts, []int{0}) || len(m.Edits) != 1 || m.Edits[0].Text != "helo" {
		t.Fatalf("edited = %+v / %+v", m.EditedParts, m.Edits)
	}
	if m.IsRetracted() {
		t.Fatal("edited message reported as retracted")
	}

	u, _ := d.GetMessage(ctx, w.unsent)
	if !u.IsRetracted() || u.Text() != "" {
		t.Fatalf("unsent = %+v", u)
	}
	u26, _ := d.GetMessage(ctx, w.unsent26)
	if !u26.IsRetracted() || u26.IsPartiallyRetracted() {
		t.Fatalf("macOS 26 unsend not detected: %+v", u26)
	}
	if m.IsPartiallyRetracted() {
		t.Fatal("edited message reported as partially unsent")
	}

	ms, _ := d.MessagesSince(ctx, 0, "any;-;quiet@example.com", 0)
	ls := ms[1].LocationShare()
	if ls == nil || ls.Direction != 1 || ms[0].LocationShare() != nil {
		t.Fatalf("location share = %+v", ls)
	}
}

func TestChatMessagesPaging(t *testing.T) {
	w := buildWorld(t)
	d := open(t, w)

	// Newest first, two at a time.
	var got []string
	q := db.ChatMessagesQuery{ChatGUID: w.dmGUID, Limit: 2}
	for {
		page, err := d.ChatMessages(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		got = append(got, guids(page)...)
		c := db.CursorFor(page[len(page)-1])
		q.Before = &c
	}
	if want := []string{w.unsent, w.edited, w.love, w.reply, w.hello}; !slices.Equal(got, want) {
		t.Fatalf("desc = %v, want %v", got, want)
	}

	// Ascending after a cursor.
	hello, _ := d.GetMessage(ctx, w.hello)
	c := db.CursorFor(hello)
	asc, _ := d.ChatMessages(ctx, db.ChatMessagesQuery{ChatGUID: w.dmGUID, After: &c, Ascending: true, Limit: 2})
	if want := []string{w.reply, w.love}; !slices.Equal(guids(asc), want) {
		t.Fatalf("asc = %v", guids(asc))
	}

	if _, err := d.ChatMessages(ctx, db.ChatMessagesQuery{ChatGUID: "nope"}); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestSameTimestampTiebreak(t *testing.T) {
	f := fixture.New(t)
	h := f.Handle("+15550000009", "iMessage")
	c := f.Chat("any;-;+15550000009", fixture.StyleDirect, "", h)
	var want []string
	for i := 0; i < 5; i++ {
		_, g := f.Message(fixture.Msg{Chat: c, Handle: h, Text: "same time", Date: at(1)})
		want = append([]string{g}, want...)
	}
	d, err := db.Open(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var got []string
	q := db.ChatMessagesQuery{ChatGUID: "any;-;+15550000009", Limit: 2}
	for {
		page, _ := d.ChatMessages(ctx, q)
		if len(page) == 0 {
			break
		}
		got = append(got, guids(page)...)
		cur := db.CursorFor(page[len(page)-1])
		q.Before = &cur
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestChats(t *testing.T) {
	w := buildWorld(t)
	d := open(t, w)

	chats, err := d.ListChats(ctx, db.ChatsQuery{IncludeParticipants: true})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, c := range chats {
		order = append(order, c.GUID)
	}
	// dm last activity 13 min > group 10 min > quiet 0 min.
	if want := []string{w.dmGUID, w.groupGUID, "any;-;quiet@example.com"}; !slices.Equal(order, want) {
		t.Fatalf("order = %v", order)
	}

	g := chats[1]
	if !g.IsGroup() || g.DisplayName != "Climbing" || !g.LastMessageAt.Equal(at(10)) {
		t.Fatalf("group = %+v", g)
	}
	var addrs []string
	for _, p := range g.Participants {
		addrs = append(addrs, p.Address)
	}
	if want := []string{"+15550000002", "+15550000001", "carol@example.com"}; !slices.Equal(addrs, want) {
		t.Fatalf("participants = %v", addrs)
	}
	if !chats[0].LastReadAt.Equal(at(3)) {
		t.Fatalf("last read = %v", chats[0].LastReadAt)
	}

	// Cursor paging.
	cur := db.ChatCursorFor(chats[0])
	rest, _ := d.ListChats(ctx, db.ChatsQuery{Before: &cur, Limit: 1})
	if len(rest) != 1 || rest[0].GUID != w.groupGUID {
		t.Fatalf("after cursor = %+v", rest)
	}

	// Search by participant address, display name, and LIKE escaping.
	for search, want := range map[string]int{"carol@": 2, "climb": 1, "%": 0, "nobody": 0} {
		res, err := d.ListChats(ctx, db.ChatsQuery{Search: search})
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != want {
			t.Errorf("search %q: %d results, want %d", search, len(res), want)
		}
	}

	c, err := d.GetChat(ctx, w.dmGUID)
	if err != nil || c.IsGroup() || len(c.Participants) != 1 {
		t.Fatalf("GetChat = %+v, %v", c, err)
	}
	if _, err := d.GetChat(ctx, "nope"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}

	last, _ := d.LastMessages(ctx, []int64{w.dm, w.group})
	if last[w.dm].GUID != w.unsent || last[w.group] == nil {
		t.Fatalf("last messages = %+v", last)
	}
}

func TestHandlesAndAttachments(t *testing.T) {
	w := buildWorld(t)
	d := open(t, w)

	hs, err := d.ListHandles(ctx, db.HandlesQuery{Search: "5550000"})
	if err != nil || len(hs) != 2 {
		t.Fatalf("handles = %+v, %v", hs, err)
	}
	page, _ := d.ListHandles(ctx, db.HandlesQuery{AfterRowID: hs[0].RowID, Limit: 1})
	if len(page) != 1 || page[0].RowID <= hs[0].RowID {
		t.Fatalf("handle paging = %+v", page)
	}
	byID, _ := d.HandlesByRowID(ctx, []int64{w.alice, w.carol})
	if byID[w.carol].Address != "carol@example.com" || len(byID) != 2 {
		t.Fatalf("by id = %+v", byID)
	}

	a, err := d.GetAttachment(ctx, "ATT-PHOTO")
	if err != nil || a.TransferName != "IMG_1.heic" || a.MessageRowID == 0 || a.TransferState != db.TransferStateComplete {
		t.Fatalf("attachment = %+v, %v", a, err)
	}
	if _, err := d.GetAttachment(ctx, "nope"); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestMimeFallback(t *testing.T) {
	f := fixture.New(t)
	h := f.Handle("x@example.com", "iMessage")
	c := f.Chat("any;-;x@example.com", fixture.StyleDirect, "", h)
	id, g := f.Message(fixture.Msg{Chat: c, Handle: h, Text: "￼"})
	f.Attachment(id, fixture.Att{GUID: "ATT-PDF", Filename: "/tmp/doc.pdf", TransferName: "doc.pdf"})
	d, _ := db.Open(f.Path)
	defer d.Close()
	m, _ := d.GetMessage(ctx, g)
	if m.Attachments[0].MimeType != "application/pdf" {
		t.Fatalf("mime = %q", m.Attachments[0].MimeType)
	}
	if m.Text() != "" {
		t.Fatalf("attachment-only text = %q", m.Text())
	}
}

func TestParseAssociatedGUID(t *testing.T) {
	cases := map[string]struct {
		guid string
		part int
	}{
		"p:2/ABC": {"ABC", 2},
		"bp:ABC":  {"ABC", 0},
		"ABC":     {"ABC", 0},
		"p:x/ABC": {"ABC", 0},
	}
	for in, want := range cases {
		g, p := db.ParseAssociatedGUID(in)
		if g != want.guid || p != want.part {
			t.Errorf("%s: got %s,%d", in, g, p)
		}
	}
}
