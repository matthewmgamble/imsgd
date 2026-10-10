package present

import (
	"time"

	"github.com/matthewmgamble/imsgd/internal/store"
)

// Send is the API shape of a send (see SendStatus in openapi.yaml).
type Send struct {
	ID        string     `json:"id"`
	ClientRef *string    `json:"client_ref"`
	Status    string     `json:"status"`
	ChatID    *string    `json:"chat_id"`
	To        *string    `json:"to"`
	Service   *string    `json:"service"`
	Error     *string    `json:"error"`
	Items     []SendItem `json:"items"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

type SendItem struct {
	Index     int     `json:"index"`
	Kind      string  `json:"kind"`
	Text      *string `json:"text"`
	Filename  *string `json:"filename"`
	Status    string  `json:"status"`
	MessageID *string `json:"message_id"`
	Error     *string `json:"error"`
}

// SendOf converts a stored send.
func SendOf(s *store.Send) Send {
	out := Send{
		ID: s.ID, ClientRef: strPtr(s.ClientRef), Status: s.Status, ChatID: strPtr(s.ChatID),
		To: strPtr(s.Recipient), Service: strPtr(s.Service), Error: strPtr(s.Error),
		Items: make([]SendItem, 0, len(s.Items)), CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
	for _, it := range s.Items {
		out.Items = append(out.Items, SendItem{
			Index: it.Index, Kind: it.Kind, Text: strPtr(it.Text), Filename: strPtr(it.Filename),
			Status: it.Status, MessageID: strPtr(it.MessageID), Error: strPtr(it.Error),
		})
	}
	return out
}
