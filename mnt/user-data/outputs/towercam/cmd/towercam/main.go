package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/michaelpeterswa/towercam/internal/config"
	"github.com/michaelpeterswa/towercam/internal/frame"
	"github.com/michaelpeterswa/towercam/internal/nws"
	"github.com/michaelpeterswa/towercam/internal/overlay"
	"github.com/michaelpeterswa/towercam/internal/protect"
	"github.com/michaelpeterswa/towercam/internal/publish"
	"github.com/michaelpeterswa/towercam/internal/wx"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := run(log); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

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

	source := wx.NewInfluxSource(cfg.InfluxURL, cfg.InfluxOrg, cfg.InfluxToken,
		cfg.InfluxBucket, cfg.InfluxStation, cfg.StaleAfter)

	conditions := nws.New(cfg.NWSStationID, cfg.NWSUserAgent)

	pub := &publish.Publisher{
		OutputDir:     cfg.OutputDir,
		ArchiveDir:    cfg.ArchiveDir,
		Quality:       cfg.JPEGQuality,
		RetentionDays: cfg.RetentionDays,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Fail loudly at startup on a bad host, key or camera ID rather than
	// discovering it as a silent gap in the feed hours later.
	if err := cam.Ping(ctx); err != nil {
		return err
	}

	go conditions.Run(ctx, cfg.NWSInterval, func(err error) {
		log.Warn("nws refresh failed", "error", err)
	})
	go prune(ctx, pub, log)

	log.Info("towercam started", "poll", cfg.PollInterval, "output", cfg.OutputDir)

	d := &daemon{
		cfg: cfg, log: log,
		cam: cam, source: source, conditions: conditions,
		renderer: renderer, pub: pub,
		params: frame.Params{
			SiteName:   cfg.SiteName,
			Elevation:  cfg.Elevation,
			StaleAfter: cfg.StaleAfter,
			Location:   loc,
			MaxFields:  theme.MaxFields,
		},
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

	reading, err := d.source.Latest(ctx)
	switch {
	case err == nil:
		d.lastGood = reading
	case errors.Is(err, wx.ErrNoData):
		// Station silent past the window. Keep publishing pictures; the
		// staleness gate below will simply drop the weather fields.
		d.log.Warn("no observation in window", "window", d.cfg.StaleAfter)
	default:
		d.log.Warn("influx query failed", "error", err)
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

	src, err := d.cam.Snapshot(ctx, true)
	if err != nil {
		return err
	}

	clean, err := d.pub.Encode(src)
	if err != nil {
		return err
	}

	conditions := ""
	if c := d.conditions.Latest(); c != nil && now.Sub(c.ObservedAt) < 90*time.Minute {
		conditions = c.Text
	}
	f := frame.Build(d.params, d.lastGood, conditions, now)

	composited, err := d.renderer.Render(src, f)
	if err != nil {
		return err
	}
	branded, err := d.pub.Encode(composited)
	if err != nil {
		return err
	}

	if err := d.pub.WriteAtomic(d.cfg.OutputDir, "latest.jpg", branded); err != nil {
		return err
	}
	if err := d.pub.WriteAtomic(d.cfg.OutputDir, "latest-clean.jpg", clean); err != nil {
		return err
	}
	if err := d.pub.Archive(now, clean); err != nil {
		d.log.Warn("archive failed", "error", err)
	}

	d.log.Info("published", "fields", len(f.Fields), "conditions", f.Conditions)
	return nil
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
