package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
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
	"github.com/michaelpeterswa/rattlecam/internal/light"
	"github.com/michaelpeterswa/rattlecam/internal/metrics"
	"github.com/michaelpeterswa/rattlecam/internal/nws"
	"github.com/michaelpeterswa/rattlecam/internal/overlay"
	"github.com/michaelpeterswa/rattlecam/internal/protect"
	"github.com/michaelpeterswa/rattlecam/internal/publish"
	"github.com/michaelpeterswa/rattlecam/internal/spool"
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

	var drainer *uploader

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

		if cfg.SpoolDir != "" {
			q, err := spool.Open(cfg.SpoolDir, int64(cfg.SpoolMaxBytes))
			if err != nil {
				return err
			}
			pub.Queue = q
			drainer = &uploader{
				spool: q, store: store, log: log, metrics: m,
				cacheControl: cfg.GCSCacheControl,
				wake:         make(chan struct{}, 1),
			}
			log.Info("queueing uploads", "spool", cfg.SpoolDir, "max_bytes", cfg.SpoolMaxBytes)
		}

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
	if drainer != nil {
		go drainer.run(ctx)
	}

	log.Info("rattlecam started", "poll", cfg.PollInterval, "output", cfg.OutputDir)

	d := &daemon{
		cfg: cfg, log: log,
		cam: cam, source: source, conditions: conditions,
		renderer: renderer, pub: pub,
		metrics: m,
		drainer: drainer,
		params:  params,
		night:   light.Detector{Enter: cfg.NightEnter, Exit: cfg.NightExit},
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
	drainer    *uploader

	lastObserved time.Time // observation time of the last reading we rendered
	lastRender   time.Time
	lastGood     *wx.Reading // survives an Influx outage

	// night is measured from each frame rather than from a clock. One state
	// drives both the annotation's colour and whether the master is archived, so
	// the two can never disagree about what time of day it is.
	night light.Detector

	warnedElevation bool
}

// seaLevelPlausible is the lowest station pressure a sea-level station would
// realistically report. Below this at sea level means a major hurricane, so on
// a station that is not in one it means the station is simply up a mountain.
const seaLevelPlausible = 950 // mb

// checkElevation warns once when the pressure implies an elevation the
// configuration does not have.
//
// SITE_ELEVATION_M defaults to 0, and at 0 the reduction is a no-op, so a
// station on a mountain publishes its raw station pressure — several inches
// below anything a local forecast shows. Nothing else catches it: the query
// succeeds, the field is present, the frame renders, and the number is simply
// wrong. A reading of 899 mb at a configured elevation of zero is not a
// plausible sea-level observation, so it is worth saying so out loud.
func (d *daemon) checkElevation(r *wx.Reading) {
	if d.warnedElevation || d.cfg.Elevation != 0 || r == nil {
		return
	}
	p, ok := r.Raw("p")
	if !ok || p >= seaLevelPlausible {
		return
	}
	d.warnedElevation = true

	unreduced, _ := r.PressureInHg(0)
	d.log.Warn("SITE_ELEVATION_M is 0 but the station pressure implies altitude; the frame will publish the unreduced value",
		"station_pressure_mb", p,
		"would_publish_in", math.Round(unreduced*100)/100,
		"hint", "set SITE_ELEVATION_M to the station's elevation in metres")
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
		d.checkElevation(reading)
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
	still, err := d.cam.Snapshot(ctx, true)
	if err != nil {
		d.metrics.FrameError(ctx, metrics.StageSnapshot)
		return err
	}
	d.metrics.SnapshotDuration(ctx, time.Since(started))

	// The camera already produced a JPEG, and nothing is drawn on the clean
	// master, so its bytes go out exactly as they arrived. Decoding and
	// re-encoding them would cost a generation of quality and roughly double
	// the size — which on this link is paid twice, once uploading and again for
	// every archived frame kept.
	clean := still.Raw

	conditions := ""
	if c := d.conditions.Latest(); c != nil && now.Sub(c.ObservedAt) < 90*time.Minute {
		conditions = c.Text
	}
	f := frame.Build(d.params, d.lastGood, conditions, now)

	// Measured off the frame we are about to publish, so the treatment always
	// matches the picture it is drawn on rather than trailing it by one cycle.
	luma := light.MeanLuma(still.Image)
	night, changed := d.night.Observe(luma)
	d.metrics.Night(ctx, night, luma)
	if changed {
		d.log.Info("light level crossed the night threshold",
			"night", night, "luma", math.Round(luma*10)/10,
			"enter_below", d.cfg.NightEnter, "exit_above", d.cfg.NightExit)
	}

	// The annotation is black ink and vanishes against a night sky, so after
	// dark it is drawn inverted. NIGHT_INVERT_ANNOTATION=false keeps the daytime
	// treatment around the clock without disturbing the archive decision below.
	f.Night = night && d.cfg.NightInvert

	composited, err := d.renderer.Render(still.Image, f)
	if err != nil {
		d.metrics.FrameError(ctx, metrics.StageRender)
		return err
	}
	branded, err := d.pub.Encode(composited)
	if err != nil {
		d.metrics.FrameError(ctx, metrics.StageEncode)
		return err
	}

	outputs := []struct {
		name string
		data []byte
	}{
		{"latest.jpg", branded},
		{"latest-clean.jpg", clean},
	}

	// A narrower copy for websites. The full frame is 4K because outlets want
	// the resolution; a browser does not, and serving 2 MB per view when 350 kB
	// is indistinguishable is the largest avoidable cost on the public path.
	// Deliberately not archived: the archive is masters, and this is derived.
	if d.cfg.WebWidth > 0 {
		web, err := d.pub.EncodeScaled(composited, d.cfg.WebWidth)
		if err != nil {
			d.metrics.FrameError(ctx, metrics.StageEncode)
			return err
		}
		outputs = append(outputs, struct {
			name string
			data []byte
		}{"latest-web.jpg", web})
	}

	for _, out := range outputs {
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

	// Archived on the same cadence around the clock. Night frames are near-black
	// and make a poor timelapse, but a frame not archived tonight cannot be
	// recovered tomorrow, and the bucket's lifecycle rules already tier old
	// masters down to nearline and colder — which is the cheaper place to solve
	// storage cost than by never writing them.
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
	// The frame is queued locally; wake the drainer so the normal case uploads
	// immediately rather than waiting for the next sweep.
	d.drainer.nudge()

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

	still, err := cam.Snapshot(ctx, true)
	if err != nil {
		return fmt.Errorf("startup render check: %w", err)
	}

	sc, err := wx.ScenarioByName("wide-values")
	if err != nil {
		return fmt.Errorf("startup render check: %w", err)
	}

	// Both treatments, because the night one first runs unattended at dusk and a
	// check that only covers daylight would let a broken inversion reach the
	// published frame hours after anyone was watching. The second render costs
	// one extra resample of the annotation at startup.
	now := time.Now()
	for _, night := range []bool{false, true} {
		f := frame.Build(p, sc.Reading(now), sc.Conditions, now)
		f.Night = night
		if _, err := r.Render(still.Image, f); err != nil {
			return fmt.Errorf("startup render check (night=%v): %w", night, err)
		}
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

// uploader moves spooled frames to the object store.
//
// It runs apart from the render loop on purpose. Rendering must not wait on a
// mountain-top network link: a frame is finished the moment it reaches local
// disk, and getting it to the bucket is a separate concern that is allowed to
// be slow, fail, and catch up later.
type uploader struct {
	spool        *spool.Spool
	store        publish.ObjectStore
	log          *slog.Logger
	metrics      *metrics.Metrics
	cacheControl string
	wake         chan struct{}
}

// nudge asks for a drain without blocking. A full channel already means one is
// pending, so dropping the signal loses nothing.
func (u *uploader) nudge() {
	if u == nil {
		return
	}
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

func (u *uploader) run(ctx context.Context) {
	// The sweep is the safety net for a drain that failed with nothing new
	// arriving to nudge it.
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()

	for {
		u.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-u.wake:
		case <-t.C:
		}
	}
}

func (u *uploader) drain(ctx context.Context) {
	entries, err := u.spool.Pending()
	if err != nil {
		u.log.Warn("reading the upload queue failed", "error", err)
		return
	}

	var sent int
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}

		data, err := e.Read()
		if err != nil {
			// The entry is unreadable, so it will never upload. Drop it rather
			// than blocking everything behind it forever.
			u.log.Warn("discarding an unreadable queued frame", "object", e.Object, "error", err)
			_ = u.spool.Done(e)
			continue
		}

		opts := publish.PutOptions{ContentType: "image/jpeg", CacheControl: u.cacheControl}
		if e.Kind == spool.Archive {
			opts.CacheControl = "public, max-age=31536000, immutable"
		}

		if err := u.store.Put(ctx, e.Object, data, opts); err != nil {
			// Almost certainly the link. Stop here and keep the order rather
			// than hammering the rest of the backlog against a dead network.
			u.log.Warn("upload failed; frames remain queued", "object", e.Object, "error", err)
			u.metrics.StoreError(ctx)
			break
		}
		if err := u.spool.Done(e); err != nil {
			u.log.Warn("removing an uploaded frame from the queue failed", "error", err)
		}
		sent++
	}

	if st, err := u.spool.Stats(); err == nil {
		u.metrics.Spool(ctx, st.Entries, st.Bytes)
		if sent > 0 && st.Entries > 0 {
			u.log.Info("working through the upload backlog", "sent", sent, "remaining", st.Entries)
		}
	}
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
