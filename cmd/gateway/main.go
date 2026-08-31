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
//	TIMELAPSE_SERVE  also serve the timelapses         (default true)
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
	"github.com/michaelpeterswa/rattlecam/internal/timelapse"
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

// newLogger builds the JSON logger, matching the daemon: same format, same
// stream, so one collector reads both without being told they differ.
func newLogger() (*slog.Logger, error) {
	level := slog.LevelInfo
	if raw := strings.TrimSpace(os.Getenv("LOG_LEVEL")); raw != "" {
		if err := level.UnmarshalText([]byte(strings.ToUpper(raw))); err != nil {
			return nil, fmt.Errorf("LOG_LEVEL: %q is not a level", raw)
		}
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	return log, nil
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
	objects := map[string]gateway.Served{
		"/latest-web.jpg": {
			Object:      key("latest-web.jpg"),
			Title:       "Web frame",
			Description: "The branded frame narrowed for websites. Start here — it is a fraction of the bytes and indistinguishable in a browser.",
			Order:       10,
		},
		"/latest.jpg": {
			Object:      key("latest.jpg"),
			Title:       "Full frame",
			Description: "The branded frame at the camera's full resolution, for outlets compositing their own graphics.",
			Order:       20,
		},
		"/latest-clean.jpg": {
			Object:      key("latest-clean.jpg"),
			Title:       "Clean frame",
			Description: "Unbranded, the camera's own bytes passed through untouched.",
			Order:       30,
		},
	}

	// The timelapses, on the same terms: fixed paths, not a listing of the dated
	// ones, so this stays a set of names rather than a way to walk the bucket.
	//
	// The names come from the same layout the job publishes with, and the same
	// variant matrix, so the gateway cannot come to serve a set of objects that
	// differs from the set that exists.
	//
	// They are held in memory like everything else here, and they are much
	// larger than a frame — the monthly alone runs to ninety megabytes or so.
	// That is the cost of the same trade the frames make: read once per build
	// rather than once per viewer. Set TIMELAPSE_SERVE=false on a host where
	// that memory is not available.
	if envBool("TIMELAPSE_SERVE", true) {
		layout := timelapse.Layout{Prefix: prefix}
		order := 100
		for _, kind := range timelapse.Kinds {
			for _, variant := range timelapse.Variants(kind) {
				// Today is rebuilt every half hour and everything else once a
				// night, so they cannot share a freshness.
				cache := "public, max-age=600"
				if kind == timelapse.Today {
					cache = "public, max-age=300"
				}
				order += 10
				for _, object := range []string{
					layout.Latest(kind, variant),
					layout.LatestGIF(kind, variant),
				} {
					objects["/"+path.Base(object)] = gateway.Served{
						Object:       object,
						CacheControl: cache,
						Title:        timelapse.Title(kind, variant),
						Description:  timelapse.Describe(kind, variant),
						Order:        order,
						// Absent until the relevant job has run once, which is
						// not a fault worth a warning every ten seconds.
						Optional: true,
					}
				}
			}
		}
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

func envBool(k string, def bool) bool {
	raw := strings.TrimSpace(os.Getenv(k))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
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
