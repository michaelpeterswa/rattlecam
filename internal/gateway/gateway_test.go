package gateway

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSource is a bucket that can be made to change, fail, or count reads.
type fakeSource struct {
	gen     atomic.Int64
	data    atomic.Value // []byte
	gets    atomic.Int64
	attrs   atomic.Int64
	failGen atomic.Bool
	failGet atomic.Bool
}

func newSource(body string) *fakeSource {
	s := &fakeSource{}
	s.gen.Store(1)
	s.data.Store([]byte(body))
	return s
}

func (s *fakeSource) publish(body string) {
	s.data.Store([]byte(body))
	s.gen.Add(1)
}

func (s *fakeSource) Generation(context.Context, string) (int64, error) {
	s.attrs.Add(1)
	if s.failGen.Load() {
		return 0, io.ErrUnexpectedEOF
	}
	return s.gen.Load(), nil
}

func (s *fakeSource) Get(context.Context, string) (Object, error) {
	s.gets.Add(1)
	if s.failGet.Load() {
		return Object{}, io.ErrUnexpectedEOF
	}
	return Object{
		Data:        s.data.Load().([]byte),
		Generation:  s.gen.Load(),
		Updated:     time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
		ContentType: "image/jpeg",
	}, nil
}

func newGateway(t *testing.T, src Source, cfg Config) *Gateway {
	t.Helper()
	if cfg.Objects == nil {
		cfg.Objects = map[string]string{"/latest.jpg": "latest.jpg"}
	}
	cfg.Log = slog.New(slog.DiscardHandler)
	g, err := New(src, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

func TestServesCachedFrame(t *testing.T) {
	src := newSource("frame")
	g := newGateway(t, src, Config{CacheControl: "no-cache"})
	g.refreshAll(context.Background())

	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/latest.jpg", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "frame" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "frame")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("Content-Type = %q", got)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("no ETag; conditional requests would be impossible")
	}
}

// The arithmetic that justifies the gateway: bucket cost must be a function of
// time, not of how many people are watching.
func TestBucketReadsDoNotScaleWithViewers(t *testing.T) {
	src := newSource("frame")
	g := newGateway(t, src, Config{})
	g.refreshAll(context.Background())

	before := src.gets.Load()
	for range 500 {
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/latest.jpg", nil))
	}
	if got := src.gets.Load() - before; got != 0 {
		t.Errorf("500 requests caused %d bucket downloads, want 0", got)
	}
}

// Only a changed generation should cost a download; polling metadata is the
// cheap half.
func TestRefreshDownloadsOnlyOnGenerationChange(t *testing.T) {
	src := newSource("frame")
	g := newGateway(t, src, Config{})

	g.refreshAll(context.Background())
	if got := src.gets.Load(); got != 1 {
		t.Fatalf("initial download count = %d, want 1", got)
	}

	for range 5 {
		g.refreshAll(context.Background())
	}
	if got := src.gets.Load(); got != 1 {
		t.Errorf("downloads = %d after unchanged refreshes, want 1", got)
	}

	src.publish("newer")
	g.refreshAll(context.Background())
	if got := src.gets.Load(); got != 2 {
		t.Errorf("downloads = %d after a new generation, want 2", got)
	}

	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/latest.jpg", nil))
	if rec.Body.String() != "newer" {
		t.Errorf("body = %q, want the new frame", rec.Body.String())
	}
}

// A poller checking every ten seconds should cost a few hundred bytes, not two
// megabytes.
func TestConditionalRequestReturns304(t *testing.T) {
	src := newSource("frame")
	g := newGateway(t, src, Config{})
	g.refreshAll(context.Background())

	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/latest.jpg", nil))
	etag := rec.Header().Get("ETag")

	req := httptest.NewRequest("GET", "/latest.jpg", nil)
	req.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec2, req)

	if rec2.Code != http.StatusNotModified {
		t.Errorf("status = %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 carried %d bytes of body", rec2.Body.Len())
	}

	// A new frame must invalidate it, or viewers would never see an update.
	src.publish("newer")
	g.refreshAll(context.Background())
	rec3 := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec3, req)
	if rec3.Code != http.StatusOK {
		t.Errorf("status = %d after a new frame, want 200", rec3.Code)
	}
}

// A hiccup reaching the bucket must not take the feed down.
func TestServesStaleFrameWhenTheBucketFails(t *testing.T) {
	src := newSource("frame")
	g := newGateway(t, src, Config{})
	g.refreshAll(context.Background())

	src.failGen.Store(true)
	src.failGet.Store(true)
	g.refreshAll(context.Background())

	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/latest.jpg", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "frame" {
		t.Errorf("status %d body %q; want the cached frame still served", rec.Code, rec.Body.String())
	}
}

// Only configured paths are served, so the gateway cannot be turned into a
// reader for arbitrary keys in a private bucket.
func TestUnlistedPathsAreNotServed(t *testing.T) {
	src := newSource("frame")
	g := newGateway(t, src, Config{})
	g.refreshAll(context.Background())

	for _, path := range []string{"/archive/2026/08/13/120000.jpg", "/../latest.jpg", "/secret.txt"} {
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("%s was served", path)
		}
	}
}

func TestRateLimit(t *testing.T) {
	src := newSource("frame")
	g := newGateway(t, src, Config{RatePerMinute: 60, Burst: 3})
	g.refreshAll(context.Background())

	var ok, limited int
	for range 10 {
		req := httptest.NewRequest("GET", "/latest.jpg", nil)
		req.RemoteAddr = "203.0.113.5:1234"
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			limited++
		}
	}
	if ok != 3 {
		t.Errorf("allowed %d requests, want the burst of 3", ok)
	}
	if limited != 7 {
		t.Errorf("limited %d requests, want 7", limited)
	}
}

// One noisy client must not lock out everyone else.
func TestRateLimitIsPerClient(t *testing.T) {
	src := newSource("frame")
	g := newGateway(t, src, Config{RatePerMinute: 60, Burst: 2})
	g.refreshAll(context.Background())

	for range 5 {
		req := httptest.NewRequest("GET", "/latest.jpg", nil)
		req.RemoteAddr = "203.0.113.5:1234"
		g.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}

	req := httptest.NewRequest("GET", "/latest.jpg", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("a second client got %d; one noisy caller locked out the rest", rec.Code)
	}
}

// Behind Caddy every request arrives from the proxy, so without honouring the
// forwarded address all clients would share a single bucket.
func TestRateLimitUsesForwardedAddress(t *testing.T) {
	src := newSource("frame")
	g := newGateway(t, src, Config{RatePerMinute: 60, Burst: 1})
	g.refreshAll(context.Background())

	send := func(fwd string) int {
		req := httptest.NewRequest("GET", "/latest.jpg", nil)
		req.RemoteAddr = "127.0.0.1:5000" // always the proxy
		req.Header.Set("X-Forwarded-For", fwd)
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	if code := send("203.0.113.5"); code != http.StatusOK {
		t.Fatalf("first client got %d", code)
	}
	if code := send("203.0.113.5"); code != http.StatusTooManyRequests {
		t.Errorf("same client got %d, want 429", code)
	}
	if code := send("198.51.100.9"); code != http.StatusOK {
		t.Errorf("different client got %d, want 200", code)
	}
}

// A proxy appends to X-Forwarded-For rather than replacing it, so a client that
// sends its own header lands left-most. Trusting that entry would hand every
// caller an unlimited supply of rate-limit identities.
func TestForgedForwardedForCannotBypassTheLimit(t *testing.T) {
	src := newSource("frame")
	g := newGateway(t, src, Config{RatePerMinute: 60, Burst: 2})
	g.refreshAll(context.Background())

	// One real client, inventing a different left-most entry every time. The
	// right-most value is what the proxy actually observed.
	var allowed int
	for i := range 20 {
		req := httptest.NewRequest("GET", "/latest.jpg", nil)
		req.RemoteAddr = "127.0.0.1:5000"
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.0.0.%d, 203.0.113.5", i))
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			allowed++
		}
	}
	if allowed > 2 {
		t.Errorf("forged headers got %d requests through a burst of 2", allowed)
	}
}

// An untrusted peer's X-Forwarded-For must be ignored entirely, or reaching the
// gateway directly would sidestep the limiter the same way.
func TestForwardedForIsIgnoredFromAnUntrustedPeer(t *testing.T) {
	src := newSource("frame")
	g := newGateway(t, src, Config{RatePerMinute: 60, Burst: 2})
	g.refreshAll(context.Background())

	var allowed int
	for i := range 20 {
		req := httptest.NewRequest("GET", "/latest.jpg", nil)
		req.RemoteAddr = "203.0.113.5:4000" // not the proxy
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.0.0.%d", i))
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			allowed++
		}
	}
	if allowed > 2 {
		t.Errorf("a direct caller got %d requests through a burst of 2", allowed)
	}
}

// Sweeping alone runs only on the refresh interval, so a flood between sweeps
// would grow the map without limit. On a 1 GB host that is a way to take the
// gateway down rather than merely abuse it.
func TestLimiterMapIsBounded(t *testing.T) {
	l := newLimiter(60, 5)
	l.maxKeys = 100

	for i := range 5000 {
		l.allow(fmt.Sprintf("10.%d.%d.%d", i/65536, (i/256)%256, i%256))
	}

	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n > 100 {
		t.Errorf("limiter holds %d buckets, over its %d cap", n, 100)
	}
}

func TestHealthz(t *testing.T) {
	src := newSource("frame")
	g := newGateway(t, src, Config{})

	// Nothing cached yet: not ready to serve, and it should say so.
	rec := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d before any frame, want 503", rec.Code)
	}

	g.refreshAll(context.Background())
	rec2 := httptest.NewRecorder()
	g.Handler().ServeHTTP(rec2, httptest.NewRequest("GET", "/healthz", nil))
	if rec2.Code != http.StatusOK {
		t.Errorf("status = %d once cached, want 200", rec2.Code)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(nil, Config{Objects: map[string]string{"/a": "a"}}); err == nil {
		t.Error("want an error with no source")
	}
	if _, err := New(newSource("x"), Config{}); err == nil {
		t.Error("want an error with no objects")
	}
}
