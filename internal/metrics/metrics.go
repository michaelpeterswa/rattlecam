// Package metrics instruments the publishing loop.
//
// The point of instrumenting this daemon is narrow and specific: a frame can
// publish cleanly every minute for hours while the weather burned into it is
// frozen, because the picture and the numbers come from different systems and
// only one of them has to fail. Nothing about the image tells you that. Only
// the age of the observation does.
package metrics

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Stage names the step of a cycle that failed, so an alert can distinguish a
// camera problem from a disk problem.
const (
	StageSnapshot = "snapshot"
	StageRender   = "render"
	StageEncode   = "encode"
	StagePublish  = "publish"
	StageArchive  = "archive"
	StageStore    = "store"
)

// Metrics holds the instruments and the small amount of state the observable
// gauges read.
type Metrics struct {
	framesPublished metric.Int64Counter
	frameErrors     metric.Int64Counter
	frameFields     metric.Int64Gauge
	snapshotSeconds metric.Float64Histogram
	influxSeconds   metric.Float64Histogram
	influxErrors    metric.Int64Counter
	nwsErrors       metric.Int64Counter
	storeErrors     metric.Int64Counter
	spoolEntries    metric.Int64Gauge
	spoolBytes      metric.Int64Gauge

	// Unix nanoseconds, zero meaning "has not happened yet". Read from the
	// collection callback on another goroutine, hence atomic.
	lastObservation atomic.Int64
	lastPublish     atomic.Int64

	// now is swappable for tests.
	now func() time.Time
}

// New registers the instruments on meter.
func New(meter metric.Meter) (*Metrics, error) {
	m := &Metrics{now: time.Now}

	// Seeded to startup rather than left at zero. Suppressing the series until
	// the first event sounds careful, but it deletes the most important metric
	// in exactly the situation it exists for: a daemon that has never reached
	// Influx reports nothing, so a threshold alert never fires and the silence
	// reads as health. Seeded, the value means "how long since we last had a
	// fresh observation", which before the first one is simply the uptime — an
	// honest answer that climbs on its own.
	started := m.now().UnixNano()
	m.lastObservation.Store(started)
	m.lastPublish.Store(started)

	var err error
	if m.framesPublished, err = meter.Int64Counter(
		"rattlecam.frames.published",
		metric.WithDescription("Frames written to the output directory."),
	); err != nil {
		return nil, fmt.Errorf("metrics: frames.published: %w", err)
	}

	if m.frameErrors, err = meter.Int64Counter(
		"rattlecam.frame.errors",
		metric.WithDescription("Failed publishing cycles, by the stage that failed."),
	); err != nil {
		return nil, fmt.Errorf("metrics: frame.errors: %w", err)
	}

	// Zero here is the signal that the staleness gate dropped everything: the
	// picture still published, but with no weather on it.
	if m.frameFields, err = meter.Int64Gauge(
		"rattlecam.frame.fields",
		metric.WithDescription("Weather fields rendered onto the most recent frame; zero means the reading was stale or absent."),
	); err != nil {
		return nil, fmt.Errorf("metrics: frame.fields: %w", err)
	}

	if m.snapshotSeconds, err = meter.Float64Histogram(
		"rattlecam.snapshot.duration",
		metric.WithDescription("Time to fetch a still from the camera."),
		metric.WithUnit("s"),
	); err != nil {
		return nil, fmt.Errorf("metrics: snapshot.duration: %w", err)
	}

	if m.influxSeconds, err = meter.Float64Histogram(
		"rattlecam.influx.duration",
		metric.WithDescription("Time to query the latest observation."),
		metric.WithUnit("s"),
	); err != nil {
		return nil, fmt.Errorf("metrics: influx.duration: %w", err)
	}

	if m.influxErrors, err = meter.Int64Counter(
		"rattlecam.influx.errors",
		metric.WithDescription("Failed observation queries, separating a silent station from a broken query."),
	); err != nil {
		return nil, fmt.Errorf("metrics: influx.errors: %w", err)
	}

	if m.storeErrors, err = meter.Int64Counter(
		"rattlecam.store.errors",
		metric.WithDescription("Failed uploads to the object store. The frame is on local disk; what viewers read is stale."),
	); err != nil {
		return nil, fmt.Errorf("metrics: store.errors: %w", err)
	}

	if m.nwsErrors, err = meter.Int64Counter(
		"rattlecam.nws.errors",
		metric.WithDescription("Failed conditions refreshes."),
	); err != nil {
		return nil, fmt.Errorf("metrics: nws.errors: %w", err)
	}

	if m.spoolEntries, err = meter.Int64Gauge(
		"rattlecam.spool.entries",
		metric.WithDescription("Frames waiting to reach the object store. Rises while the link is down."),
	); err != nil {
		return nil, fmt.Errorf("metrics: spool.entries: %w", err)
	}

	if m.spoolBytes, err = meter.Int64Gauge(
		"rattlecam.spool.bytes",
		metric.WithDescription("Bytes held on disk waiting to upload; approaches SPOOL_MAX_BYTES before eviction starts."),
	); err != nil {
		return nil, fmt.Errorf("metrics: spool.bytes: %w", err)
	}

	// These two are deliberately observable rather than set at publish time.
	//
	// An age recorded once per frame would report how old the data was at the
	// moment it was written and then sit there, frozen, looking healthy for
	// exactly as long as the process is broken. Computed at collection it keeps
	// climbing on its own, which is the entire alerting signal.
	observationAge, err := meter.Float64ObservableGauge(
		"rattlecam.observation.age",
		metric.WithDescription("Age of the weather observation behind the published frame. The metric that matters: frames can publish cleanly for hours while this climbs."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: observation.age: %w", err)
	}

	publishAge, err := meter.Float64ObservableGauge(
		"rattlecam.publish.age",
		metric.WithDescription("Time since a frame was last published."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: publish.age: %w", err)
	}

	_, err = meter.RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			// Both are seeded at startup, so these series always exist and a
			// plain threshold alert is enough — no absent() special case.
			if age, ok := m.age(&m.lastObservation); ok {
				o.ObserveFloat64(observationAge, age)
			}
			if age, ok := m.age(&m.lastPublish); ok {
				o.ObserveFloat64(publishAge, age)
			}
			return nil
		},
		observationAge, publishAge,
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register callback: %w", err)
	}

	return m, nil
}

func (m *Metrics) age(v *atomic.Int64) (float64, bool) {
	nanos := v.Load()
	if nanos == 0 {
		return 0, false
	}
	return m.now().Sub(time.Unix(0, nanos)).Seconds(), true
}

// ObservedAt records the observation time of the freshest reading held. It is
// the timestamp from the packet, not when it was fetched, so the resulting age
// covers a frozen station as well as a broken query.
func (m *Metrics) ObservedAt(t time.Time) {
	if !t.IsZero() {
		m.lastObservation.Store(t.UnixNano())
	}
}

// Published records a successful cycle and how many weather fields made it onto
// the frame.
func (m *Metrics) Published(ctx context.Context, fields int) {
	m.lastPublish.Store(m.now().UnixNano())
	m.framesPublished.Add(ctx, 1)
	m.frameFields.Record(ctx, int64(fields))
}

// FrameError records a failed cycle against the stage that failed.
func (m *Metrics) FrameError(ctx context.Context, stage string) {
	m.frameErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("stage", stage)))
}

// SnapshotDuration records how long the camera took.
func (m *Metrics) SnapshotDuration(ctx context.Context, d time.Duration) {
	m.snapshotSeconds.Record(ctx, d.Seconds())
}

// InfluxQuery records a query's duration and, when it failed, its kind. A
// silent station is counted separately from a broken query because the first is
// expected weather and the second is a bug.
func (m *Metrics) InfluxQuery(ctx context.Context, d time.Duration, kind string) {
	m.influxSeconds.Record(ctx, d.Seconds())
	if kind != "" {
		m.influxErrors.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
	}
}

// StoreError records a failed upload. Separate from FrameError because the
// frame did land locally: the feed goes stale rather than missing.
func (m *Metrics) StoreError(ctx context.Context) {
	m.storeErrors.Add(ctx, 1)
}

// Spool records the current upload backlog. A depth that climbs and never
// falls means the link is down; one that never empties means it is too slow to
// keep up, which is a different problem with the same symptom.
func (m *Metrics) Spool(ctx context.Context, entries int, bytes int64) {
	m.spoolEntries.Record(ctx, int64(entries))
	m.spoolBytes.Record(ctx, bytes)
}

// NWSError records a failed conditions refresh.
func (m *Metrics) NWSError(ctx context.Context) {
	m.nwsErrors.Add(ctx, 1)
}
