// Command gateway serves published frames from a private bucket.
//
// It runs next to the reverse proxy rather than on the tower, and reads the
// bucket with the host's own credentials. That keeps the bucket private — the
// only readers are this process and whoever you grant access to — and puts every
// request through one place that can rate limit and log.
//
//	GATEWAY_ADDR     listen address                     (default :8080)
//	GCS_BUCKET       bucket to read                     (required)
//	GCS_PREFIX       key prefix inside the bucket
//	GATEWAY_REFRESH  how often to check for a new frame (default 10s)
//	GATEWAY_RATE     requests per minute per client     (default 120, 0 disables)
//	GATEWAY_BURST    burst allowance per client         (default 20)
//	CACHE_CONTROL    header sent with every frame
//	LOG_LEVEL        debug | info | warn | error
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/michaelpeterswa/rattlecam/internal/gateway"
	"github.com/michaelpeterswa/rattlecam/internal/gcs"
)

func main() {
	log, err := newLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
		os.Exit(1)
	}
	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func newLogger() (*slog.Logger, error) {
	level := slog.LevelInfo
	if raw := strings.TrimSpace(os.Getenv("LOG_LEVEL")); raw != "" {
		if err := level.UnmarshalText([]byte(strings.ToUpper(raw))); err != nil {
			return nil, fmt.Errorf("LOG_LEVEL: %q is not a level", raw)
		}
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})), nil
}

// source adapts the storage client to what the gateway needs, keeping the
// gateway itself free of any GCS types.
type source struct{ c *gcs.Client }

func (s source) Generation(ctx context.Context, name string) (int64, error) {
	return s.c.Generation(ctx, name)
}

func (s source) Get(ctx context.Context, name string) (gateway.Object, error) {
	o, err := s.c.Get(ctx, name)
	if err != nil {
		return gateway.Object{}, err
	}
	return gateway.Object{
		Data:        o.Data,
		Generation:  o.Generation,
		Updated:     o.Updated,
		ContentType: o.ContentType,
	}, nil
}

func run(log *slog.Logger) error {
	bucket := strings.TrimSpace(os.Getenv("GCS_BUCKET"))
	if bucket == "" {
		return errors.New("GCS_BUCKET is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := gcs.New(ctx, bucket)
	if err != nil {
		return err
	}
	defer func() {
		if err := client.Close(); err != nil {
			log.Warn("closing the storage client failed", "error", err)
		}
	}()

	prefix := strings.Trim(os.Getenv("GCS_PREFIX"), "/")
	key := func(name string) string {
		if prefix == "" {
			return name
		}
		return path.Join(prefix, name)
	}

	// Only these are reachable. The archive is deliberately absent: it is a
	// bulk-download surface, and nothing about the public feed needs it.
	objects := map[string]string{
		"/latest.jpg":       key("latest.jpg"),
		"/latest-clean.jpg": key("latest-clean.jpg"),
	}

	g, err := gateway.New(source{client}, gateway.Config{
		Objects:       objects,
		Refresh:       envDuration("GATEWAY_REFRESH", 10*time.Second),
		CacheControl:  envString("CACHE_CONTROL", "no-cache, max-age=0, must-revalidate"),
		RatePerMinute: envInt("GATEWAY_RATE", 120),
		Burst:         envInt("GATEWAY_BURST", 20),
		Log:           log,
	})
	if err != nil {
		return err
	}

	go g.Run(ctx)

	addr := envString("GATEWAY_ADDR", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           g.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			log.Warn("shutdown failed", "error", err)
		}
	}()

	log.Info("gateway started", "addr", addr, "bucket", bucket, "prefix", prefix)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("shutting down")
	return nil
}

func envString(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	raw := strings.TrimSpace(os.Getenv(k))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

func envDuration(k string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(k))
	if raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return def
	}
	return v
}
