package gateway

import (
	"sync"
	"time"
)

// limiter is a per-client token bucket.
//
// The point is not to stop a determined scraper — anything served publicly can
// be pulled repeatedly — but to keep one misbehaving poller from turning a
// shared e2-micro and its egress bill into everyone else's problem.
type limiter struct {
	perMinute float64
	burst     float64

	// maxKeys bounds the map. Sweeping alone is not enough: it runs on the
	// refresh interval, and a flood of distinct addresses between two sweeps
	// would grow this without limit. On a 1 GB machine that is a way to take
	// the gateway down rather than merely abuse it.
	maxKeys int

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(perMinute, burst int) *limiter {
	if burst <= 0 {
		// A frame changes once a minute, so a handful of requests back to back
		// is normal client behaviour, not abuse.
		burst = 10
	}
	return &limiter{
		perMinute: float64(perMinute),
		burst:     float64(burst),
		maxKeys:   50_000,
		buckets:   make(map[string]*bucket),
	}
}

func (l *limiter) allow(key string) bool {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			l.evictOldestLocked(now)
		}
		l.buckets[key] = &bucket{tokens: l.burst - 1, last: now}
		return true
	}

	// Refill for the time that has passed, capped at the burst size.
	elapsed := now.Sub(b.last).Minutes()
	b.tokens += elapsed * l.perMinute
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// evictOldestLocked makes room by discarding the least recently seen entry.
//
// Dropping a bucket only ever forgives a client, never punishes one, so this is
// safe to do under pressure: the worst case is that an attacker flooding unique
// addresses resets their own limits, which is exactly the position they were in
// before the limiter existed.
func (l *limiter) evictOldestLocked(now time.Time) {
	var oldestKey string
	oldest := now
	for k, b := range l.buckets {
		if b.last.Before(oldest) {
			oldest, oldestKey = b.last, k
		}
	}
	if oldestKey != "" {
		delete(l.buckets, oldestKey)
	}
}

// sweep drops buckets that have been idle long enough to have refilled anyway.
// Without it the map grows for the lifetime of the process, one entry per
// address ever seen, which on a public endpoint is unbounded.
func (l *limiter) sweep() {
	cutoff := time.Now().Add(-10 * time.Minute)

	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.buckets {
		if b.last.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
}
