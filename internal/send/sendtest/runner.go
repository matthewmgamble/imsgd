// Package sendtest provides a fake send.Runner that behaves like Messages
// by inserting rows into a fixture chat.db.
package sendtest

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/matthewmgamble/imsgd/internal/imessage/fixture"
	"github.com/matthewmgamble/imsgd/internal/send"
)

// Runner inserts a from-me message for each send.
type Runner struct {
	F *fixture.Builder

	mu           sync.Mutex
	fail         error
	drop         bool
	messageError int64
	calls        []string
}

// FailNext makes the next call return an AppleScript-style error.
func (r *Runner) FailNext(err error) { r.mu.Lock(); r.fail = err; r.mu.Unlock() }

// DropNext makes the next call succeed without inserting a row (timeout).
func (r *Runner) DropNext() { r.mu.Lock(); r.drop = true; r.mu.Unlock() }

// ErrorNext sets message.error on the next inserted row.
func (r *Runner) ErrorNext(code int64) { r.mu.Lock(); r.messageError = code; r.mu.Unlock() }

func (r *Runner) next() (fail error, drop bool, msgErr int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fail, drop, msgErr = r.fail, r.drop, r.messageError
	r.fail, r.drop, r.messageError = nil, false, 0
	return
}

func (r *Runner) record(s string) {
	r.mu.Lock()
	r.calls = append(r.calls, s)
	r.mu.Unlock()
}

// Calls returns a copy of the recorded calls.
func (r *Runner) Calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// CallCount returns the number of calls so far.
func (r *Runner) CallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *Runner) chatRowID(guid string) (int64, error) {
	var id int64
	err := r.F.DB.QueryRow(`SELECT ROWID FROM chat WHERE guid = ?`, guid).Scan(&id)
	return id, err
}

// chatForAddress finds or creates the direct chat for an address, like
// Messages does ("any;-;<address>").
func (r *Runner) chatForAddress(addr string) int64 {
	norm := addr
	if !strings.Contains(addr, "@") {
		var b strings.Builder
		for _, c := range addr {
			if c >= '0' && c <= '9' {
				b.WriteRune(c)
			}
		}
		norm = b.String()
		if len(norm) == 10 {
			norm = "1" + norm
		}
		norm = "+" + norm
	}
	guid := "any;-;" + norm
	if id, err := r.chatRowID(guid); err == nil {
		return id
	}
	h := r.F.Handle(norm, "iMessage")
	return r.F.Chat(guid, fixture.StyleDirect, "", h)
}

func (r *Runner) insert(chat int64, text, file string, msgErr int64) {
	m := fixture.Msg{Chat: chat, FromMe: true, Text: text, Date: time.Now(), IsSent: msgErr == 0, Error: msgErr}
	if file != "" {
		m.Text = "￼"
	}
	id, _ := r.F.Message(m)
	if file != "" {
		r.F.Attachment(id, fixture.Att{Filename: file, TransferName: filepath.Base(file), MimeType: "image/jpeg"})
	}
}

func (r *Runner) do(chat func() (int64, error), kind, target, text, file string) error {
	r.record(kind + " " + target + " " + text + file)
	fail, drop, msgErr := r.next()
	if fail != nil {
		return fail
	}
	if drop {
		return nil
	}
	id, err := chat()
	if err != nil {
		return errors.New("Can’t get chat id")
	}
	r.insert(id, text, file, msgErr)
	return nil
}

func (r *Runner) TextToChat(_ context.Context, chat, text string) error {
	return r.do(func() (int64, error) { return r.chatRowID(chat) }, "text-chat", chat, text, "")
}

func (r *Runner) FileToChat(_ context.Context, chat, path string) error {
	return r.do(func() (int64, error) { return r.chatRowID(chat) }, "file-chat", chat, "", path)
}

func (r *Runner) TextToAddress(_ context.Context, addr string, _ send.Service, text string) error {
	return r.do(func() (int64, error) { return r.chatForAddress(addr), nil }, "text-addr", addr, text, "")
}

func (r *Runner) FileToAddress(_ context.Context, addr string, _ send.Service, path string) error {
	return r.do(func() (int64, error) { return r.chatForAddress(addr), nil }, "file-addr", addr, "", path)
}
