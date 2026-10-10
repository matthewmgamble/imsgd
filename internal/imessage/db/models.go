package db

import (
	"strconv"
	"strings"
	"time"
)

// Chat styles as stored in chat.style.
const (
	StyleGroup  = 43
	StyleDirect = 45
)

// Handle is a remote address (phone number or email) on a service.
type Handle struct {
	RowID           int64
	Address         string // handle.id, e.g. "+15551234567" or "a@b.com"
	Service         string // iMessage, SMS, RCS
	Country         string
	Uncanonicalized string
}

type Chat struct {
	RowID          int64
	GUID           string // e.g. "any;-;+15551234567" (macOS 26) or "iMessage;+;chat123"
	Style          int64
	ChatIdentifier string
	ServiceName    string
	DisplayName    string
	GroupID        string
	IsArchived     bool
	IsFiltered     bool
	LastReadAt     *time.Time // last_read_message_timestamp
	LastMessageAt  *time.Time // computed from chat_message_join
	Participants   []Handle   // in chat_handle_join order; loaded on request
}

func (c *Chat) IsGroup() bool { return c.Style == StyleGroup }

type Attachment struct {
	RowID          int64
	GUID           string
	MessageRowID   int64  // set when loaded for messages
	Filename       string // absolute path; "~" expanded
	UTI            string
	MimeType       string
	TransferName   string
	TotalBytes     int64
	TransferState  int64
	IsOutgoing     bool
	IsSticker      bool
	HideAttachment bool
	CreatedAt      *time.Time
	// PreviewWidth/PreviewHeight come from attribution_info ("pgensw"/
	// "pgensh"): the size of the preview Messages generated, NOT the image's
	// real dimensions (an 800×800 photo reports 600×600).
	PreviewWidth, PreviewHeight int
}

// TransferStateComplete is attachment.transfer_state when the file is on disk.
const TransferStateComplete = 5

type Message struct {
	RowID       int64
	GUID        string
	ChatRowID   int64 // 0 for the rare message without a chat
	ChatGUID    string
	HandleRowID int64 // sender for incoming; for outgoing 1:1 messages, the recipient
	OtherHandle int64 // participant affected by a group event

	Body    *Body // decoded attributedBody (or text column)
	Subject string
	Service string // iMessage, SMS, RCS

	IsFromMe        bool
	IsSent          bool
	IsDelivered     bool
	IsRead          bool
	IsFinished      bool
	IsAudioMessage  bool
	IsSystemMessage bool
	Error           int64

	Date          *time.Time
	DateRead      *time.Time
	DateDelivered *time.Time
	DatePlayed    *time.Time
	DateEdited    *time.Time
	DateRetracted *time.Time

	ItemType        int64
	GroupActionType int64
	GroupTitle      string
	ShareStatus     int64
	ShareDirection  int64

	AssociatedGUID  string
	AssociatedType  int64
	AssociatedEmoji string

	BalloonBundleID       string
	ExpressiveSendStyleID string
	ThreadOriginatorGUID  string
	ThreadOriginatorPart  string
	ReplyToGUID           string

	PartCount      int64
	EditedParts    []int
	RetractedParts []int
	Edits          []Edit

	HasAttachments bool
	Attachments    []Attachment
	ScheduleType   int64
}

// Edit is a previous version of an edited message part.
type Edit struct {
	Part int
	Date time.Time
	Text string
}

// Text returns the message's display text: attributedBody/text with
// attachment placeholders (U+FFFC) removed and surrounding space trimmed.
func (m *Message) Text() string {
	if m.Body == nil {
		return ""
	}
	return CleanText(m.Body.Text)
}

// CleanText removes object replacement characters and trims whitespace.
func CleanText(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "￼", ""))
}

// IsRetracted reports whether every part of the message was unsent.
//
// Older releases set date_retracted. On macOS 26, unsending sets
// date_edited, lists the parts under "rp" in message_summary_info and drops
// part_count to 0, leaving date_retracted unset.
func (m *Message) IsRetracted() bool {
	if m.PartCount != 0 {
		return false
	}
	return m.DateRetracted != nil || len(m.RetractedParts) > 0
}

// IsPartiallyRetracted reports whether some, but not all, parts were unsent.
func (m *Message) IsPartiallyRetracted() bool {
	return len(m.RetractedParts) > 0 && m.PartCount > 0
}

// ReactionKind names a tapback.
type ReactionKind string

const (
	ReactionLove      ReactionKind = "love"
	ReactionLike      ReactionKind = "like"
	ReactionDislike   ReactionKind = "dislike"
	ReactionLaugh     ReactionKind = "laugh"
	ReactionEmphasize ReactionKind = "emphasize"
	ReactionQuestion  ReactionKind = "question"
	ReactionEmoji     ReactionKind = "emoji"
	ReactionSticker   ReactionKind = "sticker"
)

var reactionKinds = [...]ReactionKind{
	ReactionLove, ReactionLike, ReactionDislike, ReactionLaugh,
	ReactionEmphasize, ReactionQuestion, ReactionEmoji, ReactionSticker,
}

// Reaction describes a tapback message.
type Reaction struct {
	Kind       ReactionKind
	Emoji      string // for ReactionEmoji
	Removed    bool   // the sender removed this tapback
	TargetGUID string // message the tapback applies to
	TargetPart int    // part index within the target message
}

// Reaction decodes associated_message_type. 2000–2007 add a tapback,
// 3000–3007 remove one; other values (e.g. 1000 sticker placement, 4000
// polls) are not tapbacks and return nil.
func (m *Message) Reaction() *Reaction {
	t := m.AssociatedType
	removed := false
	switch {
	case t >= 2000 && t <= 2007:
	case t >= 3000 && t <= 3007:
		removed = true
		t -= 1000
	default:
		return nil
	}
	guid, part := ParseAssociatedGUID(m.AssociatedGUID)
	r := &Reaction{
		Kind:       reactionKinds[t-2000],
		Removed:    removed,
		TargetGUID: guid,
		TargetPart: part,
	}
	if r.Kind == ReactionEmoji {
		r.Emoji = m.AssociatedEmoji
	}
	return r
}

// ParseAssociatedGUID splits associated_message_guid, which looks like
// "p:1/GUID" (part 1 of GUID), "bp:GUID" (a balloon/app message) or a bare GUID.
func ParseAssociatedGUID(s string) (guid string, part int) {
	switch {
	case strings.HasPrefix(s, "p:"):
		rest := s[2:]
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			n, err := strconv.Atoi(rest[:slash])
			if err == nil {
				return rest[slash+1:], n
			}
			return rest[slash+1:], 0
		}
		return rest, 0
	case strings.HasPrefix(s, "bp:"):
		return s[3:], 0
	}
	return s, 0
}

// GroupEventKind names a group-membership/metadata change.
type GroupEventKind string

const (
	GroupParticipantAdded   GroupEventKind = "participant_added"
	GroupParticipantRemoved GroupEventKind = "participant_removed"
	GroupParticipantLeft    GroupEventKind = "participant_left"
	GroupRenamed            GroupEventKind = "renamed"
	GroupIconChanged        GroupEventKind = "icon_changed"
	GroupIconRemoved        GroupEventKind = "icon_removed"
)

// GroupEvent describes a system row recording a group change.
type GroupEvent struct {
	Kind             GroupEventKind
	ParticipantRowID int64  // for participant events (other_handle)
	Title            string // for renamed
}

// GroupEvent decodes item_type/group_action_type, or returns nil for
// ordinary messages.
func (m *Message) GroupEvent() *GroupEvent {
	switch m.ItemType {
	case 1:
		switch m.GroupActionType {
		case 0:
			return &GroupEvent{Kind: GroupParticipantAdded, ParticipantRowID: m.OtherHandle}
		case 1:
			return &GroupEvent{Kind: GroupParticipantRemoved, ParticipantRowID: m.OtherHandle}
		}
	case 2:
		return &GroupEvent{Kind: GroupRenamed, Title: m.GroupTitle}
	case 3:
		switch m.GroupActionType {
		case 0:
			return &GroupEvent{Kind: GroupParticipantLeft, ParticipantRowID: m.HandleRowID}
		case 1:
			return &GroupEvent{Kind: GroupIconChanged}
		case 2:
			return &GroupEvent{Kind: GroupIconRemoved}
		}
	}
	return nil
}

// LocationShare describes an item_type 4 row: a notice that a participant
// started or stopped sharing their location. The meaning of the status and
// direction values is not confirmed, so they are exposed raw.
type LocationShare struct {
	Status    int64 // message.share_status
	Direction int64 // message.share_direction
}

// LocationShare returns the location-sharing notice, or nil.
func (m *Message) LocationShare() *LocationShare {
	if m.ItemType != 4 {
		return nil
	}
	return &LocationShare{Status: m.ShareStatus, Direction: m.ShareDirection}
}
