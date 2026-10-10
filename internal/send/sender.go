package send

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/matthewmgamble/imsgd/internal/events"
	"github.com/matthewmgamble/imsgd/internal/imessage/db"
	"github.com/matthewmgamble/imsgd/internal/store"
)

// Runner performs the actual sends. The default uses AppleScript; tests
// substitute a fake.
type Runner interface {
	TextToChat(ctx context.Context, chatGUID, text string) error
	FileToChat(ctx context.Context, chatGUID, path string) error
	TextToAddress(ctx context.Context, address string, svc Service, text string) error
	FileToAddress(ctx context.Context, address string, svc Service, path string) error
}

// AppleScriptRunner sends through Messages.app.
type AppleScriptRunner struct{}

func (AppleScriptRunner) TextToChat(ctx context.Context, c, t string) error {
	return TextToChat(ctx, c, t)
}
func (AppleScriptRunner) FileToChat(ctx context.Context, c, p string) error {
	return FileToChat(ctx, c, p)
}
func (AppleScriptRunner) TextToAddress(ctx context.Context, a string, s Service, t string) error {
	return TextToAddress(ctx, a, s, t)
}
func (AppleScriptRunner) FileToAddress(ctx context.Context, a string, s Service, p string) error {
	return FileToAddress(ctx, a, s, p)
}

type Options struct {
	Store  *store.Store
	Bus    *events.Bus
	Runner Runner
	// Present converts a send for events (present.Send); injected to avoid
	// an import cycle.
	Present func(*store.Send) any
	// StagingDir holds outgoing attachments. Messages references these
	// files in place, so they must never be deleted.
	StagingDir string
	// TextTimeout / AttachmentTimeout bound how long after AppleScript
	// returns we wait for the message to appear in chat.db.
	TextTimeout       time.Duration
	AttachmentTimeout time.Duration
	// SweepInterval is how often timeouts are checked (default 5s).
	SweepInterval time.Duration
	Now           func() time.Time
}

// Request describes a send.
type Request struct {
	APIKey    string
	ClientRef string
	ChatID    string // exactly one of ChatID and To
	To        string
	Service   Service
	Text      string
	Files     []StagedFile
	ID        string // optional; pre-allocated with store.NewSendID (for staging)
}

// StagedFile is an attachment already copied into the staging directory.
type StagedFile struct {
	Name string
	Path string
}

type Sender struct {
	o     Options
	queue chan *store.Send

	mu      sync.Mutex
	active  map[string]*state  // send ID → state
	byGUID  map[string]itemRef // matched message GUID → item
	pending []*pendingItem     // submitted (or about to be) but unmatched
}

type state struct {
	send    *store.Send
	settled chan struct{} // closed when every item is matched or failed
	closed  bool
}

type itemRef struct {
	sendID string
	index  int
}

type pendingItem struct {
	ref       itemRef
	chatID    string
	address   string
	kind      string
	text      string
	filename  string
	notBefore time.Time
}

func New(o Options) *Sender {
	if o.Runner == nil {
		o.Runner = AppleScriptRunner{}
	}
	if o.TextTimeout <= 0 {
		o.TextTimeout = 2 * time.Minute
	}
	if o.AttachmentTimeout <= 0 {
		o.AttachmentTimeout = 20 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.SweepInterval <= 0 {
		o.SweepInterval = 5 * time.Second
	}
	if o.Present == nil {
		o.Present = func(s *store.Send) any { return s }
	}
	return &Sender{
		o:      o,
		queue:  make(chan *store.Send, 1000),
		active: map[string]*state{},
		byGUID: map[string]itemRef{},
	}
}

// ---- validation ------------------------------------------------------------

var (
	phoneRe = regexp.MustCompile(`^\+?[0-9(][0-9 ().\-]{3,24}$`)
	emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// ValidAddress reports whether s looks like a phone number or email.
func ValidAddress(s string) bool {
	if emailRe.MatchString(s) {
		return true
	}
	return phoneRe.MatchString(s) && len(digits(s)) >= 3
}

// SanitizeFilename keeps a safe base name for staging.
func SanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == 0 || unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(strings.TrimLeft(name, "."))
	if name == "" || name == "/" {
		return "attachment"
	}
	if len(name) > 200 {
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = "" // not a real extension
		}
		name = truncateUTF8(strings.TrimSuffix(name, filepath.Ext(name)), 200-len(ext)) + ext
	}
	return name
}

// truncateUTF8 shortens s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// StagePath returns where attachment i of a send is staged.
func (s *Sender) StagePath(sendID string, i int, name string) string {
	return filepath.Join(s.o.StagingDir, s.o.Now().UTC().Format("2006-01"), sendID, fmt.Sprint(i), SanitizeFilename(name))
}

// ---- submission ------------------------------------------------------------

// Submit records a send and queues it. If the API key already used
// ClientRef, the existing send is returned with created=false.
func (s *Sender) Submit(ctx context.Context, r Request) (snd *store.Send, created bool, err error) {
	if r.ID == "" {
		r.ID = store.NewSendID()
	}
	snd = &store.Send{
		ID: r.ID, APIKey: r.APIKey, ClientRef: r.ClientRef, ChatID: r.ChatID,
		Recipient: r.To, Service: string(r.Service), Status: store.SendPending,
	}
	if r.Text != "" {
		snd.Items = append(snd.Items, store.SendItem{Kind: "text", Text: r.Text, Status: store.SendPending})
	}
	for _, f := range r.Files {
		snd.Items = append(snd.Items, store.SendItem{Kind: "attachment", Filename: filepath.Base(f.Path), Path: f.Path, Status: store.SendPending})
	}
	for i := range snd.Items {
		snd.Items[i].Index = i
	}
	if err := s.o.Store.CreateSend(ctx, snd); err != nil {
		if errors.Is(err, store.ErrConflict) {
			existing, gerr := s.o.Store.GetSendByClientRef(ctx, r.APIKey, r.ClientRef)
			return existing, false, gerr
		}
		return nil, false, err
	}
	s.mu.Lock()
	s.active[snd.ID] = &state{send: snd, settled: make(chan struct{})}
	// Copy before queueing: once queued, the worker mutates snd under s.mu.
	out := cloneSend(snd)
	s.mu.Unlock()
	select {
	case s.queue <- snd:
	default:
		s.mu.Lock()
		s.failRemaining(snd, "send queue is full")
		out = cloneSend(snd)
		s.mu.Unlock()
	}
	return out, true, nil
}

// Get returns the current state of a send.
func (s *Sender) Get(ctx context.Context, id string) (*store.Send, error) {
	s.mu.Lock()
	if st, ok := s.active[id]; ok {
		c := cloneSend(st.send)
		s.mu.Unlock()
		return c, nil
	}
	s.mu.Unlock()
	return s.o.Store.GetSend(ctx, id)
}

// Wait blocks until every item of the send is matched to a message or has
// failed, or until ctx is done. It returns the current state either way.
func (s *Sender) Wait(ctx context.Context, id string) (*store.Send, error) {
	s.mu.Lock()
	st, ok := s.active[id]
	s.mu.Unlock()
	if ok {
		select {
		case <-st.settled:
		case <-ctx.Done():
		}
	}
	return s.Get(context.WithoutCancel(ctx), id)
}

// Settled reports whether every item is matched or failed.
func Settled(snd *store.Send) bool {
	for _, it := range snd.Items {
		if it.MessageID == "" && it.Status != store.SendFailed {
			return false
		}
	}
	return true
}

func cloneSend(s *store.Send) *store.Send {
	c := *s
	c.Items = append([]store.SendItem(nil), s.Items...)
	return &c
}

// ---- worker ------------------------------------------------------------------

// Run processes the send queue and timeouts until ctx is done. Sends left
// pending by a previous run are recovered first.
func (s *Sender) Run(ctx context.Context) {
	s.recover(ctx)
	sweep := time.NewTicker(s.o.SweepInterval)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case snd := <-s.queue:
			s.process(ctx, snd)
		case <-sweep.C:
			s.sweep()
		}
	}
}

// process sends each item in order. Messages handles one AppleScript send at
// a time, so sends are serialized.
func (s *Sender) process(ctx context.Context, snd *store.Send) {
	for i := range snd.Items {
		s.mu.Lock()
		item := snd.Items[i]
		failed := snd.Status == store.SendFailed
		s.mu.Unlock()
		if failed {
			return
		}
		// Register the expected message before sending, so a row inserted
		// before AppleScript returns is still matched.
		p := &pendingItem{
			ref: itemRef{snd.ID, i}, chatID: snd.ChatID, address: snd.Recipient, kind: item.Kind,
			text: normalizeText(item.Text), filename: item.Filename, notBefore: s.o.Now().Add(-2 * time.Second),
		}
		s.mu.Lock()
		s.pending = append(s.pending, p)
		s.mu.Unlock()

		sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		err := s.run(sctx, snd, item)
		cancel()

		s.mu.Lock()
		if err != nil {
			s.removePending(p)
			slog.Warn("send failed", "send", snd.ID, "item", i, "err", err)
			snd.Items[i].Status = store.SendFailed
			snd.Items[i].Error = "Messages rejected the send: " + err.Error()
			s.failRemaining(snd, "an earlier part of this send failed")
			s.mu.Unlock()
			return
		}
		now := s.o.Now()
		if snd.Items[i].MessageID == "" { // may already be matched
			snd.Items[i].SubmittedAt = &now
		}
		s.mu.Unlock()
	}
}

func (s *Sender) run(ctx context.Context, snd *store.Send, item store.SendItem) error {
	svc := Service(snd.Service)
	if svc == "" {
		svc = ServiceIMessage
	}
	switch {
	case item.Kind == "text" && snd.ChatID != "" && snd.Recipient == "":
		return s.o.Runner.TextToChat(ctx, snd.ChatID, item.Text)
	case item.Kind == "text":
		return s.o.Runner.TextToAddress(ctx, snd.Recipient, svc, item.Text)
	case snd.ChatID != "" && snd.Recipient == "":
		return s.o.Runner.FileToChat(ctx, snd.ChatID, item.Path)
	default:
		return s.o.Runner.FileToAddress(ctx, snd.Recipient, svc, item.Path)
	}
}

// failRemaining marks unsent items failed and finalizes the send. Caller
// holds s.mu.
func (s *Sender) failRemaining(snd *store.Send, reason string) {
	for i := range snd.Items {
		it := &snd.Items[i]
		if it.MessageID == "" && it.Status != store.SendFailed && it.SubmittedAt == nil {
			it.Status = store.SendFailed
			it.Error = reason
		}
	}
	s.changed(snd)
}

// sweep fails items whose message never appeared.
func (s *Sender) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.o.Now()
	for _, st := range s.active {
		changed := false
		for i := range st.send.Items {
			it := &st.send.Items[i]
			if it.MessageID != "" || it.Status == store.SendFailed || it.SubmittedAt == nil {
				continue
			}
			timeout := s.o.TextTimeout
			if it.Kind == "attachment" {
				timeout = s.o.AttachmentTimeout
			}
			if now.Sub(*it.SubmittedAt) > timeout {
				it.Status = store.SendFailed
				it.Error = fmt.Sprintf("message did not appear in Messages within %s", timeout)
				s.removePendingRef(itemRef{st.send.ID, i})
				changed = true
			}
		}
		if changed {
			s.changed(st.send)
		}
	}
	// Forget finished sends (and long-running ones) to bound memory; their
	// final state is in the store.
	for id, st := range s.active {
		done := st.send.Status == store.SendDelivered || st.send.Status == store.SendFailed
		if (done && Settled(st.send)) || now.Sub(st.send.CreatedAt) > 24*time.Hour {
			for _, it := range st.send.Items {
				delete(s.byGUID, it.MessageID)
			}
			delete(s.active, id)
		}
	}
}

// recover resumes sends left pending by a previous process: items already
// matched keep being tracked; unmatched ones can no longer be confirmed.
func (s *Sender) recover(ctx context.Context) {
	sends, err := s.o.Store.SendsWithStatus(ctx, store.SendPending)
	if err != nil {
		slog.Error("load pending sends", "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, snd := range sends {
		if _, ok := s.active[snd.ID]; ok {
			continue // submitted by this process
		}
		st := &state{send: snd, settled: make(chan struct{})}
		s.active[snd.ID] = st
		for i := range snd.Items {
			it := &snd.Items[i]
			if it.MessageID != "" {
				s.byGUID[it.MessageID] = itemRef{snd.ID, i}
			} else if it.Status != store.SendFailed {
				it.Status = store.SendFailed
				it.Error = "imsgd restarted before this message was confirmed; check the chat to see whether it was sent"
			}
		}
		s.changed(snd)
	}
}

// ---- matching ------------------------------------------------------------------

// OnNewMessage is called by the tracker for every new chat.db row.
func (s *Sender) OnNewMessage(m *db.Message) {
	if !m.IsFromMe || m.Reaction() != nil || m.GroupEvent() != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, p := range s.pending {
		if !s.matches(p, m) {
			continue
		}
		s.pending = append(s.pending[:i], s.pending[i+1:]...)
		st, ok := s.active[p.ref.sendID]
		if !ok {
			return
		}
		it := &st.send.Items[p.ref.index]
		it.MessageID, it.MessageRowID = m.GUID, m.RowID
		s.byGUID[m.GUID] = p.ref
		if st.send.ChatID == "" {
			st.send.ChatID = m.ChatGUID
		}
		applyMessageState(it, m)
		s.changed(st.send)
		return
	}
}

// OnMessageUpdated is called by the tracker when a message changes.
func (s *Sender) OnMessageUpdated(m *db.Message, _ []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.byGUID[m.GUID]
	if !ok {
		return
	}
	st, ok := s.active[ref.sendID]
	if !ok {
		return
	}
	it := &st.send.Items[ref.index]
	before := *it
	applyMessageState(it, m)
	if *it != before {
		s.changed(st.send)
	}
}

func applyMessageState(it *store.SendItem, m *db.Message) {
	switch {
	case m.Error != 0:
		it.Status = store.SendFailed
		it.Error = fmt.Sprintf("Messages reported error %d", m.Error)
	case m.IsDelivered:
		it.Status = store.SendDelivered
	case m.IsSent:
		it.Status = store.SendSent
	default:
		it.Status = store.SendPending
	}
}

func (s *Sender) matches(p *pendingItem, m *db.Message) bool {
	if m.Date != nil && m.Date.Before(p.notBefore) {
		return false
	}
	if p.chatID != "" && p.address == "" {
		if m.ChatGUID != p.chatID {
			return false
		}
	} else if !sameAddress(chatAddress(m.ChatGUID), p.address) {
		return false
	}
	switch p.kind {
	case "text":
		return len(m.Attachments) == 0 && normalizeText(m.Text()) == p.text
	case "attachment":
		for _, a := range m.Attachments {
			if a.TransferName == p.filename {
				return true
			}
		}
	}
	return false
}

func (s *Sender) removePending(p *pendingItem) {
	for i, q := range s.pending {
		if q == p {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return
		}
	}
}

func (s *Sender) removePendingRef(ref itemRef) {
	for i, q := range s.pending {
		if q.ref == ref {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return
		}
	}
}

// changed recomputes the send status, persists it and publishes
// send.updated. Caller holds s.mu.
func (s *Sender) changed(snd *store.Send) {
	snd.Status = aggregate(snd.Items)
	snd.Error = ""
	for _, it := range snd.Items {
		if it.Status == store.SendFailed {
			snd.Error = it.Error
			break
		}
	}
	if err := s.o.Store.UpdateSend(context.Background(), snd); err != nil {
		slog.Error("persist send", "send", snd.ID, "err", err)
	}
	if st, ok := s.active[snd.ID]; ok && !st.closed && Settled(snd) {
		st.closed = true
		close(st.settled)
	}
	if s.o.Bus != nil {
		if _, err := s.o.Bus.PublishOwned(events.SendUpdated, snd.ChatID, snd.APIKey, map[string]any{"send": s.o.Present(cloneSend(snd))}); err != nil {
			slog.Error("publish send.updated", "err", err)
		}
	}
}

func aggregate(items []store.SendItem) string {
	if len(items) == 0 {
		return store.SendFailed
	}
	all := func(ok func(string) bool) bool {
		for _, it := range items {
			if !ok(it.Status) {
				return false
			}
		}
		return true
	}
	for _, it := range items {
		if it.Status == store.SendFailed {
			return store.SendFailed
		}
	}
	if all(func(s string) bool { return s == store.SendDelivered }) {
		return store.SendDelivered
	}
	if all(func(s string) bool { return s == store.SendDelivered || s == store.SendSent }) {
		return store.SendSent
	}
	return store.SendPending
}

// ---- address/text normalization --------------------------------------------------

func normalizeText(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

// chatAddress extracts the address from a direct chat GUID such as
// "any;-;+15551234567" or "iMessage;-;a@b.com" ("" for groups).
func chatAddress(guid string) string {
	parts := strings.SplitN(guid, ";", 3)
	if len(parts) != 3 || parts[1] != "-" {
		return ""
	}
	return parts[2]
}

// sameAddress compares addresses: emails case-insensitively, phone numbers
// by their last 10 digits (so "+1 (416) 555-0100" matches "4165550100").
func sameAddress(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if strings.Contains(a, "@") || strings.Contains(b, "@") {
		return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
	}
	da, dbb := digits(a), digits(b)
	if len(da) >= 10 && len(dbb) >= 10 {
		return da[len(da)-10:] == dbb[len(dbb)-10:]
	}
	return da != "" && da == dbb
}

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// EnsureDir creates the staging directory with private permissions.
func (s *Sender) EnsureDir() error { return os.MkdirAll(s.o.StagingDir, 0o700) }
