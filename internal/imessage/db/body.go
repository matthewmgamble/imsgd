package db

import (
	"strconv"
	"time"

	"github.com/matthewmgamble/imsgd/internal/imessage/keyedarchive"
	"github.com/matthewmgamble/imsgd/internal/imessage/typedstream"
)

// Body is a message's rich text content.
type Body struct {
	Text     string    // raw text including U+FFFC attachment placeholders
	Parts    []Part    // message parts in order (text runs and attachments)
	Mentions []Mention // @mentions
	Links    []Link
}

// Part is one message part. Messages can interleave text and attachments.
type Part struct {
	Index          int
	Text           string // CleanText of the part's text
	AttachmentGUID string // set for attachment parts
}

// Mention is an @mention. Start/Length are in UTF-16 code units of Body.Text.
type Mention struct {
	Start, Length int
	Address       string
}

// Link is a detected or explicit URL. Start/Length are UTF-16 code units.
type Link struct {
	Start, Length int
	URL           string
}

const (
	attrPart         = "__kIMMessagePartAttributeName"
	attrFileTransfer = "__kIMFileTransferGUIDAttributeName"
	attrMention      = "__kIMMentionConfirmedMention"
	attrLink         = "__kIMLinkAttributeName"
)

// decodeBody builds a Body from attributedBody, falling back to the text
// column. It returns nil if neither yields anything.
func decodeBody(attributed []byte, text string) *Body {
	if len(attributed) > 0 {
		if as, err := typedstream.DecodeAttributedString(attributed); err == nil {
			return bodyFromAttributed(as)
		}
	}
	if text == "" {
		return nil
	}
	return &Body{Text: text, Parts: []Part{{Index: 0, Text: CleanText(text)}}}
}

func bodyFromAttributed(as *typedstream.AttributedString) *Body {
	b := &Body{Text: as.Text}
	partIdx := map[int]int{} // part index → position in b.Parts
	for _, r := range as.Runs {
		seg := typedstream.UTF16Slice(as.Text, r.Start, r.Length)

		idx := 0
		if v, ok := r.Attributes[attrPart]; ok {
			if n, ok := v.(int64); ok {
				idx = int(n)
			}
		}
		pos, seen := partIdx[idx]
		if !seen {
			b.Parts = append(b.Parts, Part{Index: idx})
			pos = len(b.Parts) - 1
			partIdx[idx] = pos
		}
		if g, ok := r.Attributes[attrFileTransfer].(string); ok && g != "" {
			b.Parts[pos].AttachmentGUID = g
		} else {
			b.Parts[pos].Text += seg
		}

		if addr, ok := r.Attributes[attrMention].(string); ok && addr != "" {
			b.Mentions = append(b.Mentions, Mention{Start: r.Start, Length: r.Length, Address: addr})
		}
		if u, ok := r.Attributes[attrLink].(string); ok && u != "" {
			b.Links = append(b.Links, Link{Start: r.Start, Length: r.Length, URL: u})
		}
	}
	for i := range b.Parts {
		b.Parts[i].Text = CleanText(b.Parts[i].Text)
	}
	if len(b.Parts) == 0 && as.Text != "" {
		b.Parts = []Part{{Index: 0, Text: CleanText(as.Text)}}
	}
	return b
}

// summaryInfo holds what we use from message_summary_info.
type summaryInfo struct {
	editedParts    []int
	retractedParts []int
	edits          []Edit
}

// decodeSummaryInfo parses message_summary_info, a plain binary plist:
//
//	ep:  [int]                 edited part indexes
//	rp:  [int]                 retracted (unsent) part indexes
//	ec:  {"<part>": [{d: <secs since 2001>, t: <typedstream>}]}  edit history
func decodeSummaryInfo(b []byte) summaryInfo {
	var s summaryInfo
	if len(b) == 0 {
		return s
	}
	v, err := keyedarchive.Decode(b)
	if err != nil {
		return s
	}
	m, ok := v.(map[string]any)
	if !ok {
		return s
	}
	s.editedParts = intList(m["ep"])
	s.retractedParts = intList(m["rp"])
	if ec, ok := m["ec"].(map[string]any); ok {
		for k, hist := range ec {
			part, err := strconv.Atoi(k)
			if err != nil {
				continue
			}
			entries, _ := hist.([]any)
			for _, e := range entries {
				em, _ := e.(map[string]any)
				edit := Edit{Part: part}
				if d, ok := em["d"].(float64); ok {
					edit.Date = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(d * float64(time.Second)))
				}
				if t, ok := em["t"].([]byte); ok {
					if as, err := typedstream.DecodeAttributedString(t); err == nil {
						edit.Text = CleanText(as.Text)
					}
				}
				s.edits = append(s.edits, edit)
			}
		}
	}
	sortEdits(s.edits)
	return s
}

func sortEdits(e []Edit) {
	// insertion sort: edit lists are tiny
	for i := 1; i < len(e); i++ {
		for j := i; j > 0 && (e[j].Part < e[j-1].Part || (e[j].Part == e[j-1].Part && e[j].Date.Before(e[j-1].Date))); j-- {
			e[j], e[j-1] = e[j-1], e[j]
		}
	}
}

func intList(v any) []int {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]int, 0, len(arr))
	for _, e := range arr {
		if n, ok := keyedarchive.Int(e); ok {
			out = append(out, int(n))
		}
	}
	return out
}

// decodeDimensions reads image size from attachment.attribution_info.
func decodeDimensions(b []byte) (w, h int) {
	if len(b) == 0 {
		return 0, 0
	}
	v, err := keyedarchive.Decode(b)
	if err != nil {
		return 0, 0
	}
	m, _ := v.(map[string]any)
	return num(m["pgensw"]), num(m["pgensh"])
}

func num(v any) int {
	switch n := v.(type) {
	case float32:
		return int(n)
	case float64:
		return int(n)
	}
	if i, ok := keyedarchive.Int(v); ok {
		return int(i)
	}
	return 0
}
