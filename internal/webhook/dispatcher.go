// Package webhook delivers events to registered HTTP endpoints.
//
// Matching events are written to a durable queue (store.webhook_deliveries)
// before any network I/O, so deliveries survive restarts. Workers POST each
// event with an HMAC signature and retry failures with backoff.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matthewmgamble/imsgd/internal/events"
	"github.com/matthewmgamble/imsgd/internal/store"
)

// Retry schedule: the delay before attempt n+1 after n failed attempts.
// Six attempts span about 43 minutes.
var retryDelays = []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

// MaxAttempts is the number of attempts before a delivery is marked failed.
var MaxAttempts = len(retryDelays) + 1

// Headers sent with each delivery.
const (
	HeaderSignature = "X-Imsgd-Signature"
	HeaderEvent     = "X-Imsgd-Event"
	HeaderEventID   = "X-Imsgd-Event-Id"
	HeaderDelivery  = "X-Imsgd-Delivery"
)

type Options struct {
	Store     *store.Store
	Bus       *events.Bus
	Version   string
	Client    *http.Client // default: 10s timeout
	Workers   int          // concurrent deliveries; default 4
	Retention time.Duration
	Now       func() time.Time
	// KeyExists reports whether an API key name is still configured;
	// webhooks owned by revoked keys are not delivered. Nil allows all.
	KeyExists func(name string) bool
}

type Dispatcher struct {
	o      Options
	wake   chan struct{}
	mu     sync.RWMutex
	hooks  []*store.Webhook // enabled webhooks cache
	loaded bool
}

func New(o Options) *Dispatcher {
	if o.Client == nil {
		o.Client = &http.Client{
			Timeout: 10 * time.Second,
			// Never follow redirects: a 307/308 would re-POST the signed body
			// to a location the webhook owner didn't register.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if o.Retention <= 0 {
		o.Retention = 7 * 24 * time.Hour
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Dispatcher{o: o, wake: make(chan struct{}, 1)}
}

// Invalidate reloads the webhook list on the next event (call after CRUD).
func (d *Dispatcher) Invalidate() {
	d.mu.Lock()
	d.loaded = false
	d.mu.Unlock()
}

func (d *Dispatcher) enabledHooks(ctx context.Context) ([]*store.Webhook, error) {
	d.mu.RLock()
	if d.loaded {
		h := d.hooks
		d.mu.RUnlock()
		return h, nil
	}
	d.mu.RUnlock()
	all, err := d.o.Store.ListWebhooks(ctx)
	if err != nil {
		return nil, err
	}
	var enabled []*store.Webhook
	for _, w := range all {
		if w.Enabled {
			enabled = append(enabled, w)
		}
	}
	d.mu.Lock()
	d.hooks, d.loaded = enabled, true
	d.mu.Unlock()
	return enabled, nil
}

// Run consumes events and delivers them until ctx is done.
func (d *Dispatcher) Run(ctx context.Context) {
	go d.consume(ctx)
	d.deliverLoop(ctx)
}

// consume enqueues deliveries for every matching event. If the subscription
// is dropped for being slow, it resumes from the last event it handled.
func (d *Dispatcher) consume(ctx context.Context) {
	lastID := d.o.Bus.LastID()
	for ctx.Err() == nil {
		sub, backlog, resumed := d.o.Bus.Subscribe(events.Filter{}, lastID, 4096)
		if !resumed {
			slog.Warn("webhook dispatcher missed events (buffer overflow)")
		}
		for _, ev := range backlog {
			d.enqueue(ctx, ev)
			lastID = ev.ID
		}
	loop:
		for {
			select {
			case <-ctx.Done():
				sub.Close()
				return
			case <-sub.Done():
				break loop
			case ev := <-sub.C():
				d.enqueue(ctx, ev)
				lastID = ev.ID
			}
		}
	}
}

func (d *Dispatcher) enqueue(ctx context.Context, ev events.Event) {
	hooks, err := d.enabledHooks(ctx)
	if err != nil {
		slog.Error("load webhooks", "err", err)
		return
	}
	var payload []byte
	queued := false
	for _, w := range hooks {
		if w.Owner != "" && d.o.KeyExists != nil && !d.o.KeyExists(w.Owner) {
			continue // owner key revoked
		}
		f := events.Filter{Types: w.Events, ChatIDs: w.ChatIDs, Owner: w.Owner}
		if !f.Match(ev) {
			continue
		}
		if payload == nil {
			if payload, err = json.Marshal(ev); err != nil {
				slog.Error("marshal event", "err", err)
				return
			}
		}
		if _, err := d.o.Store.EnqueueDelivery(ctx, w.ID, ev.ID, ev.Type, payload); err != nil {
			slog.Error("enqueue webhook delivery", "webhook", w.ID, "err", err)
			continue
		}
		queued = true
	}
	if queued {
		select {
		case d.wake <- struct{}{}:
		default:
		}
	}
}

func (d *Dispatcher) deliverLoop(ctx context.Context) {
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-prune.C:
			if n, err := d.o.Store.PruneDeliveries(ctx, d.o.Now().Add(-d.o.Retention)); err == nil && n > 0 {
				slog.Debug("pruned webhook deliveries", "count", n)
			}
			continue
		case <-d.wake:
		case <-timer.C:
		}
		d.deliverDue(ctx)
		// Sleep until the next retry is due (or a wake-up).
		wait := time.Minute
		if next, err := d.o.Store.NextDueAt(ctx); err == nil && next != nil {
			wait = max(next.Sub(d.o.Now()), 50*time.Millisecond)
		}
		timer.Reset(wait)
	}
}

func (d *Dispatcher) deliverDue(ctx context.Context) {
	for ctx.Err() == nil {
		due, err := d.o.Store.DueDeliveries(ctx, 50)
		if err != nil {
			slog.Error("load due deliveries", "err", err)
			return
		}
		if len(due) == 0 {
			return
		}
		sem := make(chan struct{}, d.o.Workers)
		var wg sync.WaitGroup
		for _, dl := range due {
			w, err := d.o.Store.GetWebhook(ctx, dl.WebhookID)
			if err != nil {
				continue // deleted; cascade removes its deliveries
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(w *store.Webhook, dl *store.Delivery) {
				defer wg.Done()
				defer func() { <-sem }()
				d.attempt(ctx, w, dl)
			}(w, dl)
		}
		wg.Wait()
	}
}

func (d *Dispatcher) attempt(ctx context.Context, w *store.Webhook, dl *store.Delivery) {
	code, err := d.post(ctx, w, dl.ID, dl.EventID, dl.EventType, dl.Payload)
	attempts := dl.Attempts + 1
	var codePtr *int
	if code != 0 {
		codePtr = &code
	}
	if err == nil {
		d.o.Store.RecordAttempt(ctx, dl.ID, store.DeliverySucceeded, attempts, nil, codePtr, nil)
		return
	}
	msg := err.Error()
	if attempts >= MaxAttempts || !w.Enabled {
		slog.Warn("webhook delivery failed permanently", "webhook", w.ID, "event", dl.EventID, "attempts", attempts, "err", msg)
		d.o.Store.RecordAttempt(ctx, dl.ID, store.DeliveryFailed, attempts, nil, codePtr, &msg)
		return
	}
	next := d.o.Now().Add(retryDelays[attempts-1])
	slog.Info("webhook delivery failed; will retry", "webhook", w.ID, "event", dl.EventID, "attempt", attempts, "retry_at", next, "err", msg)
	d.o.Store.RecordAttempt(ctx, dl.ID, store.DeliveryPending, attempts, &next, codePtr, &msg)
}

// post sends one signed request. It returns the HTTP status (0 if none) and
// an error unless the receiver answered 2xx.
func (d *Dispatcher) post(ctx context.Context, w *store.Webhook, deliveryID, eventID, eventType string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	ts := d.o.Now().Unix()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "imsgd/"+d.o.Version)
	req.Header.Set(HeaderEvent, eventType)
	req.Header.Set(HeaderEventID, eventID)
	req.Header.Set(HeaderDelivery, deliveryID)
	req.Header.Set(HeaderSignature, Sign(w.Secret, ts, body))
	res, err := d.o.Client.Do(req)
	if err != nil {
		return 0, describeError(err)
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return res.StatusCode, fmt.Errorf("receiver returned HTTP %d", res.StatusCode)
	}
	return res.StatusCode, nil
}

// describeError reduces transport errors to a coarse class without the URL,
// so error strings don't leak details into logs or API responses.
func describeError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout():
		return errors.New("timed out")
	case strings.Contains(err.Error(), "connection refused"):
		return errors.New("connection refused")
	case strings.Contains(err.Error(), "no such host"):
		return errors.New("host not found")
	case strings.Contains(err.Error(), "tls") || strings.Contains(err.Error(), "certificate") || strings.Contains(err.Error(), "x509"):
		return errors.New("TLS error")
	}
	return errors.New("connection failed")
}

// TestResult reports a synchronous test delivery.
type TestResult struct {
	OK         bool
	StatusCode int
	Error      string
	Duration   time.Duration
}

// SendTest posts a webhook.test event to w immediately (not queued).
func (d *Dispatcher) SendTest(ctx context.Context, w *store.Webhook) TestResult {
	ev := map[string]any{
		"id":         "test-" + strconv.FormatInt(d.o.Now().UnixNano(), 36),
		"type":       "webhook.test",
		"created_at": d.o.Now().UTC(),
		"data":       map[string]any{"webhook_id": w.ID},
	}
	body, _ := json.Marshal(ev)
	start := time.Now()
	code, err := d.post(ctx, w, "test", ev["id"].(string), "webhook.test", body)
	r := TestResult{OK: err == nil, StatusCode: code, Duration: time.Since(start)}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}

// Sign computes the signature header value: "t=<unix>,v1=<hex HMAC-SHA256
// of "<unix>.<body>" keyed with the secret>".
func Sign(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// Verify checks a signature header (for receivers written in Go, and tests).
// tolerance bounds the accepted clock skew / replay window.
func Verify(secret, header string, body []byte, now time.Time, tolerance time.Duration) error {
	var ts int64
	var sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			ts, _ = strconv.ParseInt(v, 10, 64)
		case "v1":
			sig = v
		}
	}
	if ts == 0 || sig == "" {
		return fmt.Errorf("malformed signature header")
	}
	if d := now.Sub(time.Unix(ts, 0)); d > tolerance || d < -tolerance {
		return fmt.Errorf("signature timestamp outside tolerance")
	}
	want := Sign(secret, ts, body)
	if !hmac.Equal([]byte(want), []byte(fmt.Sprintf("t=%d,v1=%s", ts, sig))) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}
