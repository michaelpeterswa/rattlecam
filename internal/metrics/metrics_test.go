package metrics

import (
	"context"
	"testing"
	"time"

	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// harness wires the instruments to an in-memory reader so a test can collect
// and assert on real exported values rather than on internal state.
type harness struct {
	m      *Metrics
	reader metric.Reader
	clock  time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	h := &harness{reader: reader, clock: time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)}

	m, err := New(provider.Meter("test"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.now = func() time.Time { return h.clock }
	h.m = m

	// New seeded the timestamps from the real clock; re-seed from the fake one
	// so elapsed time is entirely under the test's control.
	m.lastObservation.Store(h.clock.UnixNano())
	m.lastPublish.Store(h.clock.UnixNano())

	return h
}

func (h *harness) advance(d time.Duration) { h.clock = h.clock.Add(d) }

// gauge returns the single data point of a float gauge, by metric name.
func (h *harness) gauge(t *testing.T, name string) (float64, bool) {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != name {
				continue
			}
			g, ok := md.Data.(metricdata.Gauge[float64])
			if !ok || len(g.DataPoints) == 0 {
				return 0, false
			}
			return g.DataPoints[0].Value, true
		}
	}
	return 0, false
}

// intGauge returns the single data point of an int gauge, by metric name.
func (h *harness) intGauge(t *testing.T, name string) (int64, bool) {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != name {
				continue
			}
			g, ok := md.Data.(metricdata.Gauge[int64])
			if !ok || len(g.DataPoints) == 0 {
				return 0, false
			}
			return g.DataPoints[0].Value, true
		}
	}
	return 0, false
}

// sum returns the total of an int counter across all attribute sets.
func (h *harness) sum(t *testing.T, name string) (int64, bool) {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			if md.Name != name {
				continue
			}
			s, ok := md.Data.(metricdata.Sum[int64])
			if !ok {
				return 0, false
			}
			var total int64
			for _, dp := range s.DataPoints {
				total += dp.Value
			}
			return total, true
		}
	}
	return 0, false
}

// The metric invariant 10 is about: it has to climb on its own between frames,
// not sit at whatever it was when the last frame happened to be written.
func TestObservationAgeClimbsWithoutAnyActivity(t *testing.T) {
	h := newHarness(t)

	h.m.ObservedAt(h.clock)
	if age, ok := h.gauge(t, "rattlecam.observation.age"); !ok || age != 0 {
		t.Fatalf("age = %v (present=%v), want 0", age, ok)
	}

	// Nothing else happens — no ticks, no frames, no calls into Metrics at all.
	h.advance(45 * time.Minute)

	age, ok := h.gauge(t, "rattlecam.observation.age")
	if !ok {
		t.Fatal("observation age disappeared")
	}
	if want := (45 * time.Minute).Seconds(); age != want {
		t.Errorf("age = %v, want %v", age, want)
	}
}

// The failure that motivates all of this: frames keep publishing on schedule
// while the weather behind them is frozen. Publishing must not reset the age.
func TestPublishingDoesNotFreshenObservationAge(t *testing.T) {
	h := newHarness(t)
	h.m.ObservedAt(h.clock)

	// An hour of perfectly healthy frames, none carrying a newer observation.
	for range 60 {
		h.advance(time.Minute)
		h.m.Published(context.Background(), 5)
	}

	age, _ := h.gauge(t, "rattlecam.observation.age")
	if want := time.Hour.Seconds(); age != want {
		t.Errorf("observation age = %v, want %v — publishing must not look like freshness", age, want)
	}
	// Meanwhile the publish age is genuinely fine, which is the whole point of
	// keeping the two apart.
	if pub, _ := h.gauge(t, "rattlecam.publish.age"); pub != 0 {
		t.Errorf("publish age = %v, want 0", pub)
	}
}

// A daemon that has never reached Influx is the worst case, and it is exactly
// when the series must still exist: a threshold alert cannot fire on a metric
// that was never emitted.
func TestObservationAgeExistsBeforeAnyObservation(t *testing.T) {
	h := newHarness(t)
	h.advance(30 * time.Minute)

	age, ok := h.gauge(t, "rattlecam.observation.age")
	if !ok {
		t.Fatal("observation age is absent before the first observation; a threshold alert would never fire")
	}
	if want := (30 * time.Minute).Seconds(); age != want {
		t.Errorf("age = %v, want the uptime %v", age, want)
	}
}

func TestObservedAtIgnoresZeroTime(t *testing.T) {
	h := newHarness(t)
	h.advance(10 * time.Minute)

	before, _ := h.gauge(t, "rattlecam.observation.age")
	h.m.ObservedAt(time.Time{})
	after, _ := h.gauge(t, "rattlecam.observation.age")

	if before != after {
		t.Errorf("a zero timestamp moved the age from %v to %v", before, after)
	}
}

// Age is measured from the observation's own timestamp, so a station that is
// reachable but reporting stale packets still shows up.
func TestObservationAgeUsesPacketTime(t *testing.T) {
	h := newHarness(t)
	h.m.ObservedAt(h.clock.Add(-20 * time.Minute))

	age, _ := h.gauge(t, "rattlecam.observation.age")
	if want := (20 * time.Minute).Seconds(); age != want {
		t.Errorf("age = %v, want %v", age, want)
	}
}

func TestPublishedRecordsFieldCount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.m.Published(ctx, 5)
	if n, ok := h.sum(t, "rattlecam.frames.published"); !ok || n != 1 {
		t.Errorf("frames published = %d (present=%v), want 1", n, ok)
	}

	// Zero fields is the staleness gate having dropped everything — a real
	// value worth recording, not an absence.
	h.m.Published(ctx, 0)
	if v, ok := h.intGauge(t, "rattlecam.frame.fields"); !ok || v != 0 {
		t.Errorf("frame fields = %v (present=%v), want 0", v, ok)
	}
	if n, _ := h.sum(t, "rattlecam.frames.published"); n != 2 {
		t.Errorf("frames published = %d, want 2", n)
	}
}

func TestErrorsAreCounted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.m.FrameError(ctx, StageSnapshot)
	h.m.FrameError(ctx, StageSnapshot)
	h.m.FrameError(ctx, StagePublish)

	if n, ok := h.sum(t, "rattlecam.frame.errors"); !ok || n != 3 {
		t.Errorf("frame errors = %d (present=%v), want 3", n, ok)
	}

	h.m.InfluxQuery(ctx, time.Second, "no_data")
	h.m.InfluxQuery(ctx, time.Second, "query_failed")
	h.m.InfluxQuery(ctx, time.Second, "") // a success must not count as an error
	if n, _ := h.sum(t, "rattlecam.influx.errors"); n != 2 {
		t.Errorf("influx errors = %d, want 2", n)
	}

	h.m.NWSError(ctx)
	if n, _ := h.sum(t, "rattlecam.nws.errors"); n != 1 {
		t.Errorf("nws errors = %d, want 1", n)
	}
}

func TestDurationsRecorded(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.m.SnapshotDuration(ctx, 250*time.Millisecond)
	h.m.InfluxQuery(ctx, 30*time.Millisecond, "")

	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			seen[md.Name] = true
		}
	}
	for _, name := range []string{"rattlecam.snapshot.duration", "rattlecam.influx.duration"} {
		if !seen[name] {
			t.Errorf("%s was not recorded", name)
		}
	}
}

// The instruments must survive being created against the no-op meter, which is
// what the daemon uses when metrics are switched off.
func TestNewWithNoopMeter(t *testing.T) {
	m, err := New(noopmetric.NewMeterProvider().Meter("test"))
	if err != nil {
		t.Fatalf("New with a no-op meter: %v", err)
	}
	ctx := context.Background()
	m.ObservedAt(time.Now())
	m.Published(ctx, 3)
	m.FrameError(ctx, StageRender)
	m.SnapshotDuration(ctx, time.Second)
	m.InfluxQuery(ctx, time.Second, "no_data")
	m.NWSError(ctx)
}
