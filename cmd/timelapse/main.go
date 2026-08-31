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
//	TIMELAPSE_MODE          nightly | today                     (default nightly)
//	TIMELAPSE_LOGO          logo object in the bucket           (default assets/logo.png)
//	TIMELAPSE_LOGO_HEIGHT   logo height as a fraction of frame  (default 0.34)
//	TIMELAPSE_LOGO_MARGIN   inset as a fraction of frame height (default 0.025)
//	TIMELAPSE_FONT          face for the timestamp; empty disables it
//	TIMELAPSE_STAMP_HEIGHT  type size as a fraction of frame height (default 0.045)
//	TIMELAPSE_STAMP_MARGIN  inset from the top-right corner       (default 0.025)
//	TIMELAPSE_GIF_WIDTH     preview width in pixels             (default 480)
//	TIMELAPSE_GIF_FPS       preview playback rate               (default 12)
//	TIMELAPSE_GIF_FRAMES    preview frame cap                   (default 200)
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
	"path"
	"path/filepath"
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
	date := flag.String("date", "", "day to build, as YYYY-MM-DD (default: yesterday, or today in -mode today)")
	mode := flag.String("mode", envString("TIMELAPSE_MODE", "nightly"), "nightly or today")
	flag.Parse()

	if *mode != "nightly" && *mode != "today" {
		return fmt.Errorf("-mode %q: want nightly or today", *mode)
	}

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
	day, err := resolveDate(*date, loc, *mode)
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

	workDir, cleanup, err := workDir()
	if err != nil {
		return err
	}
	defer cleanup(log)

	// The branding is fetched rather than baked into the image, for the same
	// reason the daemon reads its assets from disk: the artwork is the agency's
	// and is not in the repository, so an image built by CI cannot contain it.
	// Keeping it in the bucket also means changing the crest needs no new image.
	logo, err := fetchLogo(ctx, client, workDir, log)
	if err != nil {
		return err
	}

	enc := &timelapse.Encoder{
		FFmpeg:     envString("FFMPEG", "ffmpeg"),
		FFprobe:    envString("FFPROBE", "ffprobe"),
		FPS:        envInt("TIMELAPSE_FPS", timelapse.DefaultFPS),
		Width:      envInt("TIMELAPSE_WIDTH", timelapse.DefaultWidth),
		CRF:        envInt("TIMELAPSE_CRF", timelapse.DefaultCRF),
		Preset:     envString("TIMELAPSE_PRESET", timelapse.DefaultPreset),
		Timeout:    envDuration("TIMELAPSE_ENCODE_TIMEOUT", timelapse.DefaultTimeout),
		Logo:       logo,
		LogoHeight: envFloat("TIMELAPSE_LOGO_HEIGHT", timelapse.DefaultLogoHeight),
		LogoMargin: envFloat("TIMELAPSE_LOGO_MARGIN", timelapse.DefaultLogoMargin),

		// The face ships in the image: it is OFL and committed, unlike the
		// crest, so there is nothing to fetch and nothing to place by hand.
		Font:        envString("TIMELAPSE_FONT", "/usr/local/share/rattlecam/font.ttf"),
		StampHeight: envFloat("TIMELAPSE_STAMP_HEIGHT", timelapse.DefaultStampHeight),
		StampMargin: envFloat("TIMELAPSE_STAMP_MARGIN", timelapse.DefaultStampMargin),
	}

	// A font that is configured but missing would fail every encode with an
	// ffmpeg error a long way from the cause, so it is checked once here.
	if enc.Font != "" {
		if _, err := os.Stat(enc.Font); err != nil {
			return fmt.Errorf("TIMELAPSE_FONT %s: %w", enc.Font, err)
		}
	}

	b := &timelapse.Builder{
		Store: store{client},
		Layout: timelapse.Layout{
			Prefix:      os.Getenv("GCS_PREFIX"),
			Fingerprint: enc.Fingerprint(),
		},
		Enc:     enc,
		WorkDir: workDir,
		Workers: envInt("TIMELAPSE_WORKERS", 8),
		GIF: timelapse.GIFOptions{
			Width:     envInt("TIMELAPSE_GIF_WIDTH", timelapse.DefaultGIFWidth),
			FPS:       envInt("TIMELAPSE_GIF_FPS", timelapse.DefaultGIFFPS),
			MaxFrames: envInt("TIMELAPSE_GIF_FRAMES", timelapse.DefaultGIFMaxFrames),
		},
		Log: log,
	}

	if *mode == "today" {
		log.Info("timelapse started",
			"mode", "today", "day", day.Format(timelapse.DateFormat), "bucket", bucket,
			"zone", zone, "settings", enc.Fingerprint(), "branded", logo != "")
		start := time.Now()
		if err := b.Today(ctx, day); err != nil {
			return err
		}
		log.Info("timelapse finished", "elapsed", time.Since(start).Round(time.Second))
		return nil
	}

	weekDays := envInt("TIMELAPSE_WEEK_DAYS", 7)
	monthDays := envInt("TIMELAPSE_MONTH_DAYS", 30)
	if weekDays < 1 || monthDays < weekDays {
		return fmt.Errorf("TIMELAPSE_WEEK_DAYS=%d and TIMELAPSE_MONTH_DAYS=%d: the month must be at least the week",
			weekDays, monthDays)
	}

	log.Info("timelapse started",
		"mode", "nightly", "day", day.Format(timelapse.DateFormat), "bucket", bucket, "zone", zone,
		"settings", enc.Fingerprint(), "week", weekDays, "month", monthDays, "branded", logo != "")

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
	// Windows are intersected with what Ensure actually produced. Without this a
	// day the archive has never held is reported twice — once as "no frames
	// archived" and again as "the segment is unavailable" carrying an error
	// string that reads like a fault. A log that cries wolf on a known-empty day
	// is worse than one that says nothing.
	have := make(map[string]bool, len(ready))
	for _, d := range ready {
		have[d.Format(timelapse.DateFormat)] = true
	}
	usable := func(days []time.Time) []time.Time {
		out := days[:0:0]
		for _, d := range days {
			if have[d.Format(timelapse.DateFormat)] {
				out = append(out, d)
			}
		}
		return out
	}

	// Which variants exist is timelapse.Variants, the same matrix the gateway
	// reads to decide what to serve.
	windows := map[timelapse.Kind][]time.Time{
		timelapse.Yesterday: timelapse.Window(day, 1),
		timelapse.Weekly:    timelapse.Window(day, weekDays),
		timelapse.Monthly:   month,
	}

	var errs []error
	for _, kind := range []timelapse.Kind{timelapse.Yesterday, timelapse.Weekly, timelapse.Monthly} {
		for _, variant := range timelapse.Variants(kind) {
			if err := b.Product(ctx, kind, variant, day, usable(windows[kind])); err != nil {
				log.Error("building a product failed",
					"kind", string(kind), "variant", string(variant), "error", err)
				errs = append(errs, err)
			}
		}
	}

	log.Info("timelapse finished", "elapsed", time.Since(start).Round(time.Second), "failed", len(errs))
	return errors.Join(errs...)
}

// resolveDate turns the flag into a day in loc.
//
// The nightly build defaults to yesterday, because a day is only worth encoding
// into the archive of products once it is over. The today build defaults to
// today, which is the whole point of it: the result is however much of the day
// has happened, which is what a page wants to show at four in the afternoon.
func resolveDate(raw string, loc *time.Location, mode string) (time.Time, error) {
	if raw == "" {
		today := timelapse.Day(time.Now().In(loc))
		if mode == "today" {
			return today, nil
		}
		return today.AddDate(0, 0, -1), nil
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

// fetchLogo downloads the branding into the work directory, returning the local
// path or "" for an unbranded build.
//
// A logo configured explicitly and then missing is fatal, matching how the
// daemon treats its configuration: a variable that is set but unusable is an
// error rather than a silent fallback, because the alternative is publishing
// unbranded video for weeks and nobody noticing. Absent at the default path is
// simply "no branding configured".
func fetchLogo(ctx context.Context, client *gcs.Client, dir string, log *slog.Logger) (string, error) {
	object, explicit := os.LookupEnv("TIMELAPSE_LOGO")
	object = strings.TrimSpace(object)
	if !explicit {
		object = "assets/logo.png"
	}
	if object == "" {
		log.Info("no logo configured; the video will be unbranded")
		return "", nil
	}
	if prefix := strings.Trim(os.Getenv("GCS_PREFIX"), "/"); prefix != "" {
		object = path.Join(prefix, object)
	}

	obj, err := client.Get(ctx, object)
	if err != nil {
		if gcs.IsNotFound(err) && !explicit {
			log.Warn("no logo at the default path; the video will be unbranded", "object", object)
			return "", nil
		}
		return "", fmt.Errorf("TIMELAPSE_LOGO %s: %w", object, err)
	}

	dst := filepath.Join(dir, "logo"+path.Ext(object))
	if err := os.WriteFile(dst, obj.Data, 0o644); err != nil {
		return "", fmt.Errorf("timelapse: writing the logo: %w", err)
	}
	log.Info("logo fetched", "object", object, "bytes", len(obj.Data))
	return dst, nil
}

func envFloat(k string, def float64) float64 {
	raw := strings.TrimSpace(os.Getenv(k))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return def
	}
	return v
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
