// Package present converts database models into the JSON shapes of the
// public API (api/openapi.yaml). It is shared by the REST handlers and the
// event tracker so both emit identical objects.
package present

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/matthewmgamble/imsgd/internal/imessage/db"
	"github.com/matthewmgamble/imsgd/internal/media"
)

// JSON shapes. These mirror api/openapi.yaml; the contract tests fail if
// they drift.

// Presenter builds API objects, resolving handles and contact names.
type Presenter struct {
	DB    *db.DB
	Names NameResolver // optional
	// Dims reads real image dimensions (optional; nil leaves them null).
	Dims *media.DimensionCache
}

func (p *Presenter) name(addr string) *string {
	if p.Names == nil {
		return nil
	}
	return p.Names(addr)
}

type Handle struct {
	Address string  `json:"address"`
	Service string  `json:"service"`
	Name    *string `json:"name"`
}

type Chat struct {
	ID            string     `json:"id"`
	DisplayName   *string    `json:"display_name"`
	IsGroup       bool       `json:"is_group"`
	Identifier    string     `json:"identifier"`
	Service       string     `json:"service"`
	IsArchived    bool       `json:"is_archived"`
	LastMessageAt *time.Time `json:"last_message_at"`
	LastReadAt    *time.Time `json:"last_read_at"`
	Participants  []Handle   `json:"participants,omitempty"`
	LastMessage   **Message  `json:"last_message,omitempty"`
}

type Message struct {
	ID            string         `json:"id"`
	RowID         int64          `json:"rowid"`
	ChatID        *string        `json:"chat_id"`
	Type          string         `json:"type"`
	FromMe        bool           `json:"from_me"`
	Sender        *Handle        `json:"sender"`
	Text          string         `json:"text"`
	Subject       *string        `json:"subject"`
	Service       string         `json:"service"`
	Parts         []Part         `json:"parts"`
	Mentions      []Mention      `json:"mentions"`
	Links         []Link         `json:"links"`
	Attachments   []Attachment   `json:"attachments"`
	CreatedAt     time.Time      `json:"created_at"`
	DeliveredAt   *time.Time     `json:"delivered_at"`
	ReadAt        *time.Time     `json:"read_at"`
	EditedAt      *time.Time     `json:"edited_at"`
	IsSent        bool           `json:"is_sent"`
	IsDelivered   bool           `json:"is_delivered"`
	IsRead        bool           `json:"is_read"`
	Error         int64          `json:"error"`
	Unsent        bool           `json:"unsent"`
	UnsentParts   []int          `json:"unsent_parts"`
	Edits         []Edit         `json:"edits"`
	Reaction      *Reaction      `json:"reaction"`
	GroupEvent    *GroupEvent    `json:"group_event"`
	LocationShare *LocationShare `json:"location_share"`
	ReplyToID     *string        `json:"reply_to_id"`
	Effect        *string        `json:"effect"`
	App           *string        `json:"app"`
}

type Part struct {
	Index        int     `json:"index"`
	Text         string  `json:"text"`
	AttachmentID *string `json:"attachment_id"`
}

type Mention struct {
	Start   int    `json:"start"`
	Length  int    `json:"length"`
	Address string `json:"address"`
}

type Link struct {
	Start  int    `json:"start"`
	Length int    `json:"length"`
	URL    string `json:"url"`
}

type Edit struct {
	Part     int       `json:"part"`
	EditedAt time.Time `json:"edited_at"`
	Text     string    `json:"text"`
}

type Reaction struct {
	Kind       string  `json:"kind"`
	Emoji      *string `json:"emoji"`
	Removed    bool    `json:"removed"`
	TargetID   string  `json:"target_id"`
	TargetPart int     `json:"target_part"`
}

type GroupEvent struct {
	Kind        string  `json:"kind"`
	Participant *Handle `json:"participant"`
	Title       *string `json:"title"`
}

type LocationShare struct {
	Status    int64 `json:"status"`
	Direction int64 `json:"direction"`
}

type Attachment struct {
	ID         string     `json:"id"`
	MessageID  *string    `json:"message_id"`
	Filename   string     `json:"filename"`
	MimeType   string     `json:"mime_type"`
	UTI        *string    `json:"uti"`
	Size       int64      `json:"size"`
	Width      *int       `json:"width"`
	Height     *int       `json:"height"`
	IsSticker  bool       `json:"is_sticker"`
	IsOutgoing bool       `json:"is_outgoing"`
	Available  bool       `json:"available"`
	LivePhoto  bool       `json:"live_photo"`
	CreatedAt  *time.Time `json:"created_at"`
}

// NameResolver maps an address to a contact name (nil if unknown).
type NameResolver func(address string) *string

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Handle converts a handle.
func (p *Presenter) Handle(h db.Handle) Handle {
	return Handle{Address: h.Address, Service: h.Service, Name: p.name(h.Address)}
}

// Chat converts a chat; participants are included when requested.
func (p *Presenter) Chat(c *db.Chat, withParticipants bool) Chat {
	out := Chat{
		ID:            c.GUID,
		DisplayName:   strPtr(c.DisplayName),
		IsGroup:       c.IsGroup(),
		Identifier:    c.ChatIdentifier,
		Service:       c.ServiceName,
		IsArchived:    c.IsArchived,
		LastMessageAt: c.LastMessageAt,
		LastReadAt:    c.LastReadAt,
	}
	if withParticipants {
		out.Participants = make([]Handle, 0, len(c.Participants))
		for _, h := range c.Participants {
			out.Participants = append(out.Participants, p.Handle(h))
		}
	}
	return out
}

// Attachment converts an attachment belonging to message messageGUID.
func (p *Presenter) Attachment(a *db.Attachment, messageGUID string) Attachment {
	out := Attachment{
		ID:         a.GUID,
		MessageID:  strPtr(messageGUID),
		Filename:   a.TransferName,
		MimeType:   a.MimeType,
		UTI:        strPtr(a.UTI),
		Size:       a.TotalBytes,
		IsSticker:  a.IsSticker,
		IsOutgoing: a.IsOutgoing,
		CreatedAt:  a.CreatedAt,
	}
	fi, ok := attachmentStat(a)
	if !ok {
		return out
	}
	out.Available = true
	out.Size = fi.Size()
	if strings.HasPrefix(a.MimeType, "image/") {
		if p.Dims != nil {
			if w, h, ok := p.Dims.Get(a.Filename, fi); ok {
				out.Width, out.Height = &w, &h
			}
		}
		out.LivePhoto = media.LivePhotoVideo(a.Filename) != ""
	}
	return out
}

// AttachmentFile reports the on-disk size and whether the file can be served.
// total_bytes in chat.db does not always match the file, so the real size is
// preferred when available.
func AttachmentFile(a *db.Attachment) (int64, bool) {
	fi, ok := attachmentStat(a)
	if !ok {
		return 0, false
	}
	return fi.Size(), true
}

func attachmentStat(a *db.Attachment) (os.FileInfo, bool) {
	if a.Filename == "" || a.TransferState != db.TransferStateComplete {
		return nil, false
	}
	fi, err := os.Stat(a.Filename)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	return fi, true
}

// AttachmentAvailable reports whether the attachment's file can be served.
func AttachmentAvailable(a *db.Attachment) bool {
	_, ok := AttachmentFile(a)
	return ok
}

// Messages converts messages, resolving sender and participant handles in
// one query.
func (p *Presenter) Messages(ctx context.Context, msgs []*db.Message) ([]Message, error) {
	ids := map[int64]bool{}
	for _, m := range msgs {
		if m.HandleRowID != 0 {
			ids[m.HandleRowID] = true
		}
		if m.OtherHandle != 0 {
			ids[m.OtherHandle] = true
		}
	}
	list := make([]int64, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	handles, err := p.DB.HandlesByRowID(ctx, list)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, p.message(m, handles))
	}
	return out, nil
}

func (p *Presenter) message(m *db.Message, handles map[int64]db.Handle) Message {
	handle := func(id int64) *Handle {
		h, ok := handles[id]
		if !ok {
			return nil
		}
		j := p.Handle(h)
		return &j
	}

	out := Message{
		ID:          m.GUID,
		RowID:       m.RowID,
		ChatID:      strPtr(m.ChatGUID),
		Type:        "message",
		FromMe:      m.IsFromMe,
		Text:        m.Text(),
		Subject:     strPtr(m.Subject),
		Service:     m.Service,
		Parts:       []Part{},
		Mentions:    []Mention{},
		Links:       []Link{},
		Attachments: []Attachment{},
		DeliveredAt: m.DateDelivered,
		ReadAt:      m.DateRead,
		EditedAt:    m.DateEdited,
		IsSent:      m.IsSent,
		IsDelivered: m.IsDelivered,
		IsRead:      m.IsRead,
		Error:       m.Error,
		Unsent:      m.IsRetracted(),
		UnsentParts: []int{},
		Edits:       []Edit{},
		ReplyToID:   strPtr(m.ThreadOriginatorGUID),
		Effect:      strPtr(m.ExpressiveSendStyleID),
		App:         strPtr(m.BalloonBundleID),
	}
	if m.Date != nil {
		out.CreatedAt = *m.Date
	}
	if !m.IsFromMe {
		out.Sender = handle(m.HandleRowID)
	}
	if m.IsRetracted() {
		out.EditedAt = nil // the edit timestamp records the unsend
	}
	if m.RetractedParts != nil {
		out.UnsentParts = m.RetractedParts
	}

	hidden := map[string]bool{}
	for i := range m.Attachments {
		a := &m.Attachments[i]
		if a.HideAttachment {
			hidden[a.GUID] = true
			continue
		}
		out.Attachments = append(out.Attachments, p.Attachment(a, m.GUID))
	}
	if m.Body != nil {
		for _, p := range m.Body.Parts {
			if p.AttachmentGUID != "" && hidden[p.AttachmentGUID] {
				continue
			}
			out.Parts = append(out.Parts, Part{Index: p.Index, Text: p.Text, AttachmentID: strPtr(p.AttachmentGUID)})
		}
		for _, mt := range m.Body.Mentions {
			out.Mentions = append(out.Mentions, Mention{Start: mt.Start, Length: mt.Length, Address: mt.Address})
		}
		for _, l := range m.Body.Links {
			out.Links = append(out.Links, Link{Start: l.Start, Length: l.Length, URL: l.URL})
		}
	}
	if !out.Unsent {
		for _, e := range m.Edits {
			out.Edits = append(out.Edits, Edit{Part: e.Part, EditedAt: e.Date, Text: e.Text})
		}
	}

	switch {
	case m.Reaction() != nil:
		r := m.Reaction()
		out.Type = "reaction"
		out.Reaction = &Reaction{
			Kind: string(r.Kind), Emoji: strPtr(r.Emoji), Removed: r.Removed,
			TargetID: r.TargetGUID, TargetPart: r.TargetPart,
		}
	case m.GroupEvent() != nil:
		ge := m.GroupEvent()
		out.Type = "group_event"
		gj := &GroupEvent{Kind: string(ge.Kind), Title: strPtr(ge.Title)}
		// "left" records the leaver as the sender; when that is the account
		// owner there is no handle.
		if ge.ParticipantRowID != 0 && !(ge.Kind == db.GroupParticipantLeft && m.IsFromMe) {
			gj.Participant = handle(ge.ParticipantRowID)
		}
		out.GroupEvent = gj
	case m.LocationShare() != nil:
		ls := m.LocationShare()
		out.Type = "location_share"
		out.LocationShare = &LocationShare{Status: ls.Status, Direction: ls.Direction}
	}
	return out
}
