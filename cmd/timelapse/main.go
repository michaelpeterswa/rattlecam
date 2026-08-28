// Command timelapse builds the day, week and month videos from the archive.
//
// It runs once a night, next to the bucket rather than on the tower: the
// archive it reads is a couple of gigabytes a month and the tower's uplink is
// the one thing the whole design is trying not to spend.
//
// Each day is encoded once into a pair of segments — the whole day, and the
// daylight hours on their own — which are kept in the bucket. The three
// products are then stitched from those segments by copying the compressed
// stream, so a month costs one day's encoding rather than thirty. A run that
// finds segments missing builds them, which means the first run bootstraps the
// whole window and a run after an outage repairs itself with no separate
// command and no state outside the bucket.
//
//	GCS_BUCKET              bucket holding the archive          (required)
//	GCS_PREFIX              key prefix inside the bucket
//	TZ                      zone the archive's days are in      (default America/Los_Angeles)
//	FFMPEG                  ffmpeg binary                       (default ffmpeg, from PATH)
//	TIMELAPSE_FPS           output frame rate                   (default 24)
//	TIMELAPSE_WIDTH         output width in pixels              (default 1280)
//	TIMELAPSE_CRF           x264 quality, lower is larger       (default 25)
//	TIMELAPSE_PRESET        x264 preset                         (default slow)
//	TIMELAPSE_WEEK_DAYS     days in the weekly                  (default 7)
//	TIMELAPSE_MONTH_DAYS    days in the monthly                 (default 30)
//	TIMELAPSE_BUILD_BUDGET  segments one run may encode         (default 31)
//	TIMELAPSE_WORKERS       concurrent downloads and decodes    (default 8)
//	TIMELAPSE_WORKDIR       scratch space                       (default a temp directory)
//	LOG_LEVEL               debug | info | warn | error
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/michaelpeterswa/rattlecam/internal/gcs"
	"github.com/michaelpeterswa/rattlecam/internal/publish"
	"github.com/michaelpeterswa/rattlecam/internal/timelapse"
)

func main() {
	log, err := newLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "timelapse: %v\n", err)
		os.Exit(1)
	}
	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// newLogger matches the daemon and the gateway: JSON on stdout, so one
// collector reads all three without being told they differ.
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

// store adapts the storage client to what the builder needs, keeping the
// timelapse package free of any GCS types.
type store struct{ c *gcs.Client }

func (s store) List(ctx context.Context, prefix string) ([]string, error) {
	return s.c.List(ctx, prefix)
}

func (s store) Get(ctx context.Context, name string) ([]byte, error) {
	o, err := s.c.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return o.Data, nil
}

func (s store) Put(ctx context.Context, name string, data []byte, contentType, cacheControl string) error {
	return s.c.Put(ctx, name, data, publish.PutOptions{
		ContentType:  contentType,
		CacheControl: cacheControl,
	})
}

func run(log *slog.Logger) error {
	date := flag.String("date", "", "day to build, as YYYY-MM-DD (default: yesterday)")
	flag.Parse()

	bucket := strings.TrimSpace(os.Getenv("GCS_BUCKET"))
	if bucket == "" {
		return errors.New("GCS_BUCKET is required")
	}

	zone := envString("TZ", "America/Los_Angeles")
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return fmt.Errorf("TZ %q: %w", zone, err)
	}

	// The archive's day directories are named in this zone, because the daemon
	// writing them runs with this TZ set. Resolving the date anywhere else would
	// build a "day" that starts at five in the afternoon.
	day, err := resolveDate(*date, loc)
	if err != nil {
		return err
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

	enc := &timelapse.Encoder{
		FFmpeg:  envString("FFMPEG", "ffmpeg"),
		FPS:     envInt("TIMELAPSE_FPS", timelapse.DefaultFPS),
		Width:   envInt("TIMELAPSE_WIDTH", timelapse.DefaultWidth),
		CRF:     envInt("TIMELAPSE_CRF", timelapse.DefaultCRF),
		Preset:  envString("TIMELAPSE_PRESET", timelapse.DefaultPreset),
		Timeout: envDuration("TIMELAPSE_ENCODE_TIMEOUT", timelapse.DefaultTimeout),
	}

	workDir, cleanup, err := workDir()
	if err != nil {
		return err
	}
	defer cleanup(log)

	b := &timelapse.Builder{
		Store: store{client},
		Layout: timelapse.Layout{
			Prefix:      os.Getenv("GCS_PREFIX"),
			Fingerprint: enc.Fingerprint(),
		},
		Enc:     enc,
		WorkDir: workDir,
		Workers: envInt("TIMELAPSE_WORKERS", 8),
		Log:     log,
	}

	weekDays := envInt("TIMELAPSE_WEEK_DAYS", 7)
	monthDays := envInt("TIMELAPSE_MONTH_DAYS", 30)
	if weekDays < 1 || monthDays < weekDays {
		return fmt.Errorf("TIMELAPSE_WEEK_DAYS=%d and TIMELAPSE_MONTH_DAYS=%d: the month must be at least the week",
			weekDays, monthDays)
	}

	log.Info("timelapse started",
		"day", day.Format(timelapse.DateFormat), "bucket", bucket, "zone", zone,
		"settings", enc.Fingerprint(), "week", weekDays, "month", monthDays)

	start := time.Now()

	// The month is the widest window, so ensuring it covers what the week and
	// the day need as well — one pass, one listing, one download of each
	// segment.
	month := timelapse.Window(day, monthDays)
	ready, err := b.Ensure(ctx, month, envInt("TIMELAPSE_BUILD_BUDGET", 31))
	if err != nil {
		return err
	}
	if len(ready) == 0 {
		return errors.New("timelapse: no days are available to build from")
	}
	log.Info("segments ready", "days", len(ready), "of", len(month), "elapsed", time.Since(start).Round(time.Second))

	// Each product is attempted even if an earlier one failed. They are
	// independent, and a week that cannot be built is no reason to skip the
	// month that could have been.
	var errs []error
	for _, p := range []struct {
		kind    timelapse.Kind
		days    []time.Time
		variant timelapse.Variant
	}{
		{timelapse.Daily, timelapse.Window(day, 1), timelapse.Full},
		{timelapse.Weekly, timelapse.Window(day, weekDays), timelapse.Full},
		// The month is the one that drops the night. A third of every day is a
		// black rectangle, and thirty of them is forty minutes of nothing in a
		// video meant to show a season moving.
		{timelapse.Monthly, month, timelapse.DaylightOnly},
	} {
		if err := b.Product(ctx, p.kind, day, p.days, p.variant); err != nil {
			log.Error("building a product failed", "kind", string(p.kind), "error", err)
			errs = append(errs, err)
		}
	}

	log.Info("timelapse finished", "elapsed", time.Since(start).Round(time.Second), "failed", len(errs))
	return errors.Join(errs...)
}

// resolveDate turns the flag into a day in loc, defaulting to yesterday.
//
// Yesterday rather than today because a day is only worth encoding once it is
// over: run against today and the result is however much of it had happened by
// half past midnight.
func resolveDate(raw string, loc *time.Location) (time.Time, error) {
	if raw == "" {
		return timelapse.Day(time.Now().In(loc)).AddDate(0, 0, -1), nil
	}
	t, err := time.ParseInLocation(timelapse.DateFormat, raw, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("-date %q: want YYYY-MM-DD", raw)
	}
	return t, nil
}

// workDir returns the scratch directory and how to clean it up.
//
// A configured directory is left alone on exit — it may be a mounted volume the
// operator wants to inspect — while a temp directory this process created is
// removed, because on Cloud Run it is memory.
func workDir() (string, func(*slog.Logger), error) {
	if dir := strings.TrimSpace(os.Getenv("TIMELAPSE_WORKDIR")); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", nil, fmt.Errorf("TIMELAPSE_WORKDIR: %w", err)
		}
		return dir, func(*slog.Logger) {}, nil
	}

	dir, err := os.MkdirTemp("", "rattlecam-timelapse-*")
	if err != nil {
		return "", nil, fmt.Errorf("timelapse: scratch directory: %w", err)
	}
	return dir, func(log *slog.Logger) {
		if err := os.RemoveAll(dir); err != nil {
			log.Warn("clearing the scratch directory failed", "dir", dir, "error", err)
		}
	}, nil
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
