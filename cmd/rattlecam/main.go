package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"alpineworks.io/ootel"
	"go.opentelemetry.io/otel"

	"github.com/michaelpeterswa/rattlecam/internal/config"
	"github.com/michaelpeterswa/rattlecam/internal/frame"
	"github.com/michaelpeterswa/rattlecam/internal/gcs"
	"github.com/michaelpeterswa/rattlecam/internal/metrics"
	"github.com/michaelpeterswa/rattlecam/internal/nws"
	"github.com/michaelpeterswa/rattlecam/internal/overlay"
	"github.com/michaelpeterswa/rattlecam/internal/protect"
	"github.com/michaelpeterswa/rattlecam/internal/publish"
	"github.com/michaelpeterswa/rattlecam/internal/wx"
)

// serviceName is the instrumentation scope every metric is recorded under.
const serviceName = "rattlecam"

func main() {
	// Building the logger is the one failure that cannot be logged, so it is
	// the only thing that reports itself straight to stderr.
	log, err := newLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rattlecam: %v\n", err)
		os.Exit(1)
	}

	if err := run(log); err != nil {
		// A bad environment is a list of problems rather than one sentence, and
		// a structured handler should emit it as a list rather than as a
		// sentence with separators buried in it.
		var cfgErr *config.Error
		if errors.As(err, &cfgErr) {
			log.Error("fatal", "error", "invalid configuration", "problems", cfgErr.Problems)
		} else {
			log.Error("fatal", "error", err)
		}
		os.Exit(1)
	}
}

// newLogger builds the logger from the environment.
//
// This is read here rather than through config.Load because the logger has to
// exist before configuration can be loaded — config.Load's own failures are
// reported through it.
//
//	LOG_LEVEL   debug | info | warn | error   (default info)
//	LOG_FORMAT  text | json                   (default text)
func newLogger() (*slog.Logger, error) {
	level := slog.LevelInfo
	if raw := strings.TrimSpace(os.Getenv("LOG_LEVEL")); raw != "" {
		if err := level.UnmarshalText([]byte(strings.ToUpper(raw))); err != nil {
			return nil, fmt.Errorf("LOG_LEVEL: %q is not a level (want debug, info, warn or error)", raw)
		}
	}

	opts := &slog.HandlerOptions{Level: level}

	switch format := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_FORMAT"))); format {
	case "", "text":
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	default:
		return nil, fmt.Errorf("LOG_FORMAT: %q is not a format (want text or json)", format)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	for _, w := range cfg.Warnings {
		log.Warn(w)
	}

	m, shutdownTelemetry, err := startTelemetry(ctxBackground(), cfg, log)
	if err != nil {
		return err
	}
	defer func() {
		if err := shutdownTelemetry(ctxBackground()); err != nil {
			log.Warn("telemetry shutdown failed", "error", err)
		}
	}()

	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return fmt.Errorf("timezone %q: %w", cfg.Timezone, err)
	}

	cam, err := protect.New(cfg.ProtectHost, cfg.ProtectAPIKey, cfg.ProtectCameraID, cfg.ProtectCertSHA)
	if err != nil {
		return err
	}
	if cfg.ProtectCertSHA == "" {
		log.Warn("PROTECT_CERT_SHA256 unset; console TLS certificate is not verified")
	}

	theme, err := overlay.LoadTheme(cfg.ThemePath)
	if err != nil {
		return err
	}
	renderer, err := overlay.NewRenderer(cfg.FontPath, cfg.BoldFontPath, cfg.LogoPath, theme)
	if err != nil {
		return err
	}
	// The annotation is optional; a missing default file is not an error, but a
	// path the operator set explicitly and got wrong is.
	if err := loadAnnotation(renderer, cfg.AnnotationPath, log); err != nil {
		return err
	}

	source := wx.NewInfluxSource(cfg.InfluxURL, cfg.InfluxOrg, cfg.InfluxToken,
		cfg.InfluxBucket, cfg.InfluxStation, cfg.StaleAfter)

	conditions := nws.New(cfg.NWSStationID, cfg.NWSUserAgent)

	pub := &publish.Publisher{
		OutputDir:      cfg.OutputDir,
		ArchiveDir:     cfg.ArchiveDir,
		Quality:        cfg.JPEGQuality,
		RetentionDays:  cfg.RetentionDays,
		ObjectPrefix:   cfg.GCSPrefix,
		CacheControl:   cfg.GCSCacheControl,
		ArchiveToStore: cfg.GCSArchive,
	}

	if cfg.GCSBucket != "" {
		store, err := gcs.New(ctxBackground(), cfg.GCSBucket)
		if err != nil {
			return err
		}
		defer func() {
			if err := store.Close(); err != nil {
				log.Warn("closing the object store failed", "error", err)
			}
		}()
		pub.Store = store
		log.Info("publishing to object store",
			"bucket", store.Bucket(), "prefix", cfg.GCSPrefix, "archive", cfg.GCSArchive)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Fail loudly at startup on a bad host, key or camera ID rather than
	// discovering it as a silent gap in the feed hours later.
	if err := cam.Ping(ctx); err != nil {
		return err
	}

	params := frame.Params{
		SiteName:   cfg.SiteName,
		Credit:     cfg.Credit,
		Elevation:  cfg.Elevation,
		StaleAfter: cfg.StaleAfter,
		Location:   loc,
		MaxFields:  theme.MaxFields,
	}

	if err := selfTest(ctx, cam, renderer, params); err != nil {
		return err
	}
	log.Info("startup render check passed")

	go conditions.Run(ctx, cfg.NWSInterval, func(err error) {
		log.Warn("nws refresh failed", "error", err)
		m.NWSError(ctx)
	})
	go prune(ctx, pub, log)

	log.Info("rattlecam started", "poll", cfg.PollInterval, "output", cfg.OutputDir)

	d := &daemon{
		cfg: cfg, log: log,
		cam: cam, source: source, conditions: conditions,
		renderer: renderer, pub: pub,
		metrics: m,
		params:  params,
	}
	d.loop(ctx)
	return nil
}

type daemon struct {
	cfg        *config.Config
	log        *slog.Logger
	params     frame.Params
	cam        *protect.Client
	source     *wx.InfluxSource
	conditions *nws.Client
	renderer   *overlay.Renderer
	pub        *publish.Publisher
	metrics    *metrics.Metrics

	lastObserved time.Time // observation time of the last reading we rendered
	lastRender   time.Time
	lastGood     *wx.Reading // survives an Influx outage
}

// loop polls Influx and renders when a genuinely new observation lands, with a
// floor so we never render faster than the station reports, and a ceiling so a
// dead station or a dead Influx can't freeze the published image.
func (d *daemon) loop(ctx context.Context) {
	t := time.NewTicker(d.cfg.PollInterval)
	defer t.Stop()

	d.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			d.log.Info("shutting down")
			return
		case <-t.C:
			d.tick(ctx)
		}
	}
}

func (d *daemon) tick(ctx context.Context) {
	now := time.Now()

	started := time.Now()
	reading, err := d.source.Latest(ctx)
	switch {
	case err == nil:
		d.lastGood = reading
		d.metrics.InfluxQuery(ctx, time.Since(started), "")
		// The packet's own timestamp, so observation age reflects a frozen
		// station just as surely as a broken query.
		d.metrics.ObservedAt(reading.ObservedAt)
	case errors.Is(err, wx.ErrNoData):
		// Station silent past the window. Keep publishing pictures; the
		// staleness gate below will simply drop the weather fields.
		d.log.Warn("no observation in window", "window", d.cfg.StaleAfter)
		d.metrics.InfluxQuery(ctx, time.Since(started), "no_data")
	default:
		d.log.Warn("influx query failed", "error", err)
		d.metrics.InfluxQuery(ctx, time.Since(started), "query_failed")
	}

	fresh := reading != nil && reading.ObservedAt.After(d.lastObserved)
	forced := now.Sub(d.lastRender) >= d.cfg.MaxFrameAge

	if !fresh && !forced {
		return
	}
	if fresh && now.Sub(d.lastRender) < d.cfg.MinFrameGap && !forced {
		return
	}

	if err := d.renderFrame(ctx, now); err != nil {
		d.log.Error("frame failed", "error", err)
		return
	}

	d.lastRender = now
	if reading != nil {
		d.lastObserved = reading.ObservedAt
	}
}

func (d *daemon) renderFrame(ctx context.Context, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	started := time.Now()
	src, err := d.cam.Snapshot(ctx, true)
	if err != nil {
		d.metrics.FrameError(ctx, metrics.StageSnapshot)
		return err
	}
	d.metrics.SnapshotDuration(ctx, time.Since(started))

	clean, err := d.pub.Encode(src)
	if err != nil {
		d.metrics.FrameError(ctx, metrics.StageEncode)
		return err
	}

	conditions := ""
	if c := d.conditions.Latest(); c != nil && now.Sub(c.ObservedAt) < 90*time.Minute {
		conditions = c.Text
	}
	f := frame.Build(d.params, d.lastGood, conditions, now)

	composited, err := d.renderer.Render(src, f)
	if err != nil {
		d.metrics.FrameError(ctx, metrics.StageRender)
		return err
	}
	branded, err := d.pub.Encode(composited)
	if err != nil {
		d.metrics.FrameError(ctx, metrics.StageEncode)
		return err
	}

	for _, out := range []struct {
		name string
		data []byte
	}{
		{"latest.jpg", branded},
		{"latest-clean.jpg", clean},
	} {
		// A store failure means the frame reached local disk but not the bucket
		// viewers read from. That is a stale feed, not a lost frame, so it is
		// logged and counted rather than aborting the cycle.
		if err := d.pub.Publish(ctx, out.name, out.data); err != nil {
			if errors.Is(err, publish.ErrStore) {
				d.log.Warn("upload failed; serving may be stale", "object", out.name, "error", err)
				d.metrics.StoreError(ctx)
				continue
			}
			d.metrics.FrameError(ctx, metrics.StagePublish)
			return err
		}
	}

	if err := d.pub.Archive(ctx, now, clean); err != nil {
		// Archiving is for timelapses later; failing it must not stop the feed.
		d.log.Warn("archive failed", "error", err)
		if errors.Is(err, publish.ErrStore) {
			d.metrics.StoreError(ctx)
		} else {
			d.metrics.FrameError(ctx, metrics.StageArchive)
		}
	}

	// Field count is recorded here rather than derived from the reading: this is
	// what actually reached the frame, after the staleness gate had its say.
	d.metrics.Published(ctx, len(f.Fields))
	d.log.Info("published", "fields", len(f.Fields), "conditions", f.Conditions)
	return nil
}

// ctxBackground exists so the telemetry lifetime is not tied to the signal
// context: shutdown has to flush after that context is already cancelled.
func ctxBackground() context.Context { return context.Background() }

// startTelemetry brings up the meter provider and registers the instruments.
//
// With metrics disabled it still returns a usable *metrics.Metrics, backed by
// OTel's no-op meter, so the call sites throughout the loop stay free of nil
// checks and the daemon behaves identically either way.
func startTelemetry(ctx context.Context, cfg *config.Config, log *slog.Logger) (*metrics.Metrics, func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }

	if !cfg.MetricsEnabled {
		m, err := metrics.New(otel.Meter(serviceName))
		if err != nil {
			return nil, nil, err
		}
		log.Info("metrics disabled")
		return m, noop, nil
	}

	client := ootel.NewOotelClient(
		ootel.WithMetricConfig(ootel.NewMetricConfig(
			true, cfg.MetricsExporter, cfg.MetricsPort,
		)),
	)

	shutdown, err := client.Init(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry: %w", err)
	}

	// Instruments must be created from the provider ootel installed, not from
	// one captured earlier, or they record into a discarded meter.
	m, err := metrics.New(otel.Meter(serviceName))
	if err != nil {
		return nil, nil, errors.Join(err, shutdown(ctx))
	}

	log.Info("metrics enabled", "exporter", cfg.MetricsExporter, "port", cfg.MetricsPort)
	return m, shutdown, nil
}

// selfTest renders one real frame at startup and throws it away.
//
// Everything the renderer needs — the fonts, the crest, the annotation's aspect,
// every placement value in the theme — is only exercised when a frame is drawn.
// Without this, a typo in the theme or a missing font fails on every single
// frame while the process sits there looking healthy, and the feed just stops
// advancing. Better to refuse to start.
//
// It uses a real snapshot rather than a synthetic image because the checks that
// matter most depend on the camera's actual resolution and aspect, and the
// widest built-in scenario because a layout that only survives a mild afternoon
// should fail here rather than on the first stormy day.
func selfTest(ctx context.Context, cam *protect.Client, r *overlay.Renderer, p frame.Params) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	src, err := cam.Snapshot(ctx, true)
	if err != nil {
		return fmt.Errorf("startup render check: %w", err)
	}

	s, err := wx.ScenarioByName("wide-values")
	if err != nil {
		return fmt.Errorf("startup render check: %w", err)
	}

	now := time.Now()
	if _, err := r.Render(src, frame.Build(p, s.Reading(now), s.Conditions, now)); err != nil {
		return fmt.Errorf("startup render check: %w", err)
	}
	return nil
}

// loadAnnotation installs the peak-outline layer. The default path is a
// convention rather than a requirement, so its absence is only worth a log
// line; anything else is a real misconfiguration.
func loadAnnotation(r *overlay.Renderer, path string, log *slog.Logger) error {
	if path == "" {
		return nil
	}
	err := r.LoadAnnotation(path)
	if err == nil {
		log.Info("annotation layer loaded", "path", path)
		return nil
	}
	if errors.Is(err, os.ErrNotExist) && path == "assets/annotation.png" {
		log.Info("no annotation layer; continuing without one", "path", path)
		return nil
	}
	return err
}

func prune(ctx context.Context, pub *publish.Publisher, log *slog.Logger) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := pub.Prune(time.Now()); err != nil {
				log.Warn("prune failed", "error", err)
			}
		}
	}
}
