// Package gateway serves published frames from a private bucket.
//
// It exists so the bucket does not have to be public. Readers reach the gateway,
// the gateway reaches the bucket with its own credentials, and the object store
// is never exposed. That also puts every request through one place, which is
// where rate limiting and logging can live.
//
// The other half of its job is arithmetic. A frame is a couple of megabytes and
// changes once a minute; fetching it per request would mean a bucket read and a
// full transfer for every viewer. Holding it in memory and re-reading only when
// the generation changes makes bucket cost a function of time rather than of
// audience.
package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Source is the bucket, narrowed to what serving needs.
type Source interface {
	Generation(ctx context.Context, name string) (int64, error)
	Get(ctx context.Context, name string) (Object, error)
}

// Object is a fetched frame.
type Object struct {
	Data        []byte
	Generation  int64
	Updated     time.Time
	ContentType string
}

// Config describes what to serve and how hard callers may pull on it.
type Config struct {
	// Objects maps a request path to an object name in the bucket. Only these
	// paths are served; anything else is a 404, so the gateway cannot be used
	// to read arbitrary keys.
	Objects map[string]string

	// Refresh is how often the generation is checked. It bounds how stale a
	// served frame can be, and it is the only per-time cost the bucket sees.
	Refresh time.Duration

	// CacheControl is sent with every frame.
	CacheControl string

	// RatePerMinute and Burst cap a single client. Zero disables limiting.
	RatePerMinute int
	Burst         int

	Log *slog.Logger
}

// Gateway serves frames from memory, refreshed in the background.
type Gateway struct {
	src Source
	cfg Config
	log *slog.Logger

	mu      sync.RWMutex
	cached  map[string]Object
	limiter *limiter
}

func New(src Source, cfg Config) (*Gateway, error) {
	if src == nil {
		return nil, fmt.Errorf("gateway: source is required")
	}
	if len(cfg.Objects) == 0 {
		return nil, fmt.Errorf("gateway: at least one object must be served")
	}
	if cfg.Refresh <= 0 {
		cfg.Refresh = 10 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}

	g := &Gateway{
		src:    src,
		cfg:    cfg,
		log:    cfg.Log,
		cached: make(map[string]Object, len(cfg.Objects)),
	}
	if cfg.RatePerMinute > 0 {
		g.limiter = newLimiter(cfg.RatePerMinute, cfg.Burst)
	}
	return g, nil
}

// Run keeps the cache current until ctx is done.
func (g *Gateway) Run(ctx context.Context) {
	g.refreshAll(ctx)

	t := time.NewTicker(g.cfg.Refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.refreshAll(ctx)
		}
	}
}

func (g *Gateway) refreshAll(ctx context.Context) {
	for _, object := range g.cfg.Objects {
		g.refresh(ctx, object)
	}
	if g.limiter != nil {
		g.limiter.sweep()
	}
}

// refresh downloads an object only when its generation has moved.
func (g *Gateway) refresh(ctx context.Context, object string) {
	gen, err := g.src.Generation(ctx, object)
	if err != nil {
		// A failure here leaves the previous frame in place, which is the point:
		// a hiccup reaching the bucket should not take the feed down.
		g.log.Warn("checking the object failed; serving the cached frame", "object", object, "error", err)
		return
	}

	g.mu.RLock()
	current, ok := g.cached[object]
	g.mu.RUnlock()
	if ok && current.Generation == gen {
		return
	}

	obj, err := g.src.Get(ctx, object)
	if err != nil {
		g.log.Warn("fetching the object failed; serving the cached frame", "object", object, "error", err)
		return
	}

	g.mu.Lock()
	g.cached[object] = obj
	g.mu.Unlock()
	g.log.Debug("frame refreshed", "object", object, "generation", obj.Generation, "bytes", len(obj.Data))
}

// Handler routes requests to cached frames.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		// Healthy means able to serve, which means something is cached.
		g.mu.RLock()
		n := len(g.cached)
		g.mu.RUnlock()
		if n == 0 {
			http.Error(w, "no frame cached yet", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	for path, object := range g.cfg.Objects {
		mux.Handle("GET "+path, g.serve(object))
	}
	return mux
}

func (g *Gateway) serve(object string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.limiter != nil && !g.limiter.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}

		g.mu.RLock()
		obj, ok := g.cached[object]
		g.mu.RUnlock()
		if !ok {
			http.Error(w, "no frame available", http.StatusServiceUnavailable)
			return
		}

		contentType := obj.ContentType
		if contentType == "" {
			contentType = "image/jpeg"
		}

		// The generation is a precise version, so it makes a strong validator.
		// A poller checking every ten seconds then costs a few hundred bytes
		// instead of two megabytes, which is most of the egress saved.
		etag := `"` + strconv.FormatInt(obj.Generation, 10) + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", g.cfg.CacheControl)
		w.Header().Set("Content-Type", contentType)
		if !obj.Updated.IsZero() {
			w.Header().Set("Last-Modified", obj.Updated.UTC().Format(http.TimeFormat))
		}

		if match := r.Header.Get("If-None-Match"); match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.Header().Set("Content-Length", strconv.Itoa(len(obj.Data)))
		_, _ = w.Write(obj.Data)
	})
}

// clientIP prefers the address Caddy forwarded, since the gateway only ever
// sees the proxy otherwise and every client would share one bucket.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		// Left-most entry is the original client.
		if i := len(fwd); i > 0 {
			for j, c := range fwd {
				if c == ',' {
					return trimSpace(fwd[:j])
				}
			}
			return trimSpace(fwd)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
