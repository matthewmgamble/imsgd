package api

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// rateLimiter is a per-key token bucket.
type rateLimiter struct {
	mu      sync.Mutex
	perSec  float64
	burst   float64
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMinute, burst int) *rateLimiter {
	return &rateLimiter{perSec: float64(perMinute) / 60, burst: float64(burst), buckets: map[string]*bucket{}, now: time.Now}
}

// allow takes a token for key, or reports how long until one is available.
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.perSec)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.perSec * float64(time.Second))
}

// connLimiter caps concurrent long-lived connections per key.
type connLimiter struct {
	mu    sync.Mutex
	max   int
	count map[string]int
}

func (c *connLimiter) acquire(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.count[key] >= c.max {
		return false
	}
	c.count[key]++
	return true
}

func (c *connLimiter) release(key string) {
	c.mu.Lock()
	c.count[key]--
	c.mu.Unlock()
}

func tooManyRequests(w http.ResponseWriter, msg string, retry time.Duration) {
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
	}
	writeError(w, http.StatusTooManyRequests, codeRateLimited, msg)
}
