package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// setRequired satisfies the five mandatory variables so a test can focus on
// whatever else it is checking. t.Setenv also isolates each test's environment.
func setRequired(t *testing.T) {
	t.Helper()
	for _, kv := range [][2]string{
		{"PROTECT_HOST", "192.168.1.1"},
		{"PROTECT_API_KEY", "key"},
		{"PROTECT_CAMERA_ID", "cam"},
		{"INFLUX_ORG", "org"},
		{"INFLUX_TOKEN", "token"},
	} {
		t.Setenv(kv[0], kv[1])
	}
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if c.JPEGQuality != 92 {
		t.Errorf("JPEGQuality = %d, want 92", c.JPEGQuality)
	}
	if c.PollInterval != 15*time.Second {
		t.Errorf("PollInterval = %s, want 15s", c.PollInterval)
	}
	if c.StaleAfter != 10*time.Minute {
		t.Errorf("StaleAfter = %s, want 10m", c.StaleAfter)
	}
	if c.InfluxBucket != "weather" {
		t.Errorf("InfluxBucket = %q, want weather", c.InfluxBucket)
	}
	if c.Elevation != 0 {
		t.Errorf("Elevation = %v, want 0", c.Elevation)
	}
}

func TestRequiredVariables(t *testing.T) {
	_, err := Load()
	if err == nil {
		t.Fatal("want an error with nothing set")
	}
	// Every missing variable should be reported at once, not one per restart.
	for _, k := range []string{
		"PROTECT_HOST", "PROTECT_API_KEY", "PROTECT_CAMERA_ID", "INFLUX_ORG", "INFLUX_TOKEN",
	} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error does not mention %s:\n%v", k, err)
		}
	}
}

// The bug this package had: a set-but-malformed value silently became the
// default. For SITE_ELEVATION_M that meant elevation 0, which turns the pressure
// reduction into a no-op and publishes raw station pressure — a wrong number on
// a public frame with nothing logged anywhere.
func TestMalformedElevationIsRejected(t *testing.T) {
	setRequired(t)
	t.Setenv("SITE_ELEVATION_M", "1,200")

	c, err := Load()
	if err == nil {
		t.Fatalf("a comma in SITE_ELEVATION_M was accepted, yielding elevation %v", c.Elevation)
	}
	if !strings.Contains(err.Error(), "SITE_ELEVATION_M") {
		t.Errorf("error does not name the variable:\n%v", err)
	}
	if !strings.Contains(err.Error(), `"1,200"`) {
		t.Errorf("error does not quote the offending value:\n%v", err)
	}
}

func TestMalformedValuesRejected(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"JPEG_QUALITY", "ninetytwo"},
		{"RETENTION_DAYS", "7 days"},
		{"SITE_ELEVATION_M", "1200 m"}, // a unit suffix is not a number
		{"SITE_ELEVATION_M", "NaN"},
		{"POLL_INTERVAL", "15"}, // no unit
		{"MAX_FRAME_AGE", "3 min"},
		{"STALE_AFTER", "ten"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			setRequired(t)
			t.Setenv(tc.key, tc.val)

			if _, err := Load(); err == nil {
				t.Fatalf("%s=%q was accepted", tc.key, tc.val)
			} else if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error does not name %s:\n%v", tc.key, err)
			}
		})
	}
}

// Unset must still mean "use the default" — that is the whole point of the
// distinction, and blanking a variable in a compose file counts as unset.
func TestUnsetAndEmptyTakeTheDefault(t *testing.T) {
	setRequired(t)
	t.Setenv("JPEG_QUALITY", "")
	t.Setenv("POLL_INTERVAL", "   ")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.JPEGQuality != 92 {
		t.Errorf("JPEGQuality = %d, want the default 92", c.JPEGQuality)
	}
	if c.PollInterval != 15*time.Second {
		t.Errorf("PollInterval = %s, want the default 15s", c.PollInterval)
	}
}

func TestValidValuesAccepted(t *testing.T) {
	setRequired(t)
	t.Setenv("SITE_ELEVATION_M", "1200.5")
	t.Setenv("JPEG_QUALITY", "88")
	t.Setenv("POLL_INTERVAL", "30s")
	t.Setenv("STALE_AFTER", "5m")
	t.Setenv("MIN_FRAME_GAP", "0s") // a zero floor is legitimate
	t.Setenv("RETENTION_DAYS", "30")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Elevation != 1200.5 {
		t.Errorf("Elevation = %v, want 1200.5", c.Elevation)
	}
	if c.JPEGQuality != 88 {
		t.Errorf("JPEGQuality = %d, want 88", c.JPEGQuality)
	}
	if c.MinFrameGap != 0 {
		t.Errorf("MinFrameGap = %s, want 0", c.MinFrameGap)
	}
	if c.RetentionDays != 30 {
		t.Errorf("RetentionDays = %d, want 30", c.RetentionDays)
	}
}

// MAX_FRAME_AGE is checked once per poll, so a value below POLL_INTERVAL is a
// ceiling that cannot fire. Clamping makes the config honest about what will
// actually happen.
func TestMaxFrameAgeClampsToPollInterval(t *testing.T) {
	setRequired(t)
	t.Setenv("POLL_INTERVAL", "15m")
	t.Setenv("MAX_FRAME_AGE", "3m")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxFrameAge != 15*time.Minute {
		t.Errorf("MaxFrameAge = %s, want it clamped to 15m", c.MaxFrameAge)
	}
	if len(c.Warnings) != 1 {
		t.Fatalf("got %d warnings, want 1: %v", len(c.Warnings), c.Warnings)
	}
	for _, want := range []string{"MAX_FRAME_AGE", "POLL_INTERVAL", "3m", "15m"} {
		if !strings.Contains(c.Warnings[0], want) {
			t.Errorf("warning omits %q: %s", want, c.Warnings[0])
		}
	}
}

func TestMaxFrameAgeLeftAloneWhenItCanFire(t *testing.T) {
	for _, tc := range []struct{ poll, maxAge string }{
		{"15s", "3m"},  // the defaults: the ceiling is well above the poll
		{"30s", "30s"}, // equal is fine; the check is >=
	} {
		t.Run(tc.poll+"/"+tc.maxAge, func(t *testing.T) {
			setRequired(t)
			t.Setenv("POLL_INTERVAL", tc.poll)
			t.Setenv("MAX_FRAME_AGE", tc.maxAge)

			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			want, _ := time.ParseDuration(tc.maxAge)
			if c.MaxFrameAge != want {
				t.Errorf("MaxFrameAge = %s, want %s unchanged", c.MaxFrameAge, want)
			}
			if len(c.Warnings) != 0 {
				t.Errorf("unexpected warnings: %v", c.Warnings)
			}
		})
	}
}

// The defaults must not warn, or every startup would carry noise.
func TestDefaultsProduceNoWarnings(t *testing.T) {
	setRequired(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Warnings) != 0 {
		t.Errorf("defaults warned: %v", c.Warnings)
	}
}

func TestMetricsSettings(t *testing.T) {
	setRequired(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.MetricsEnabled {
		t.Error("metrics should default to on; the observation-age metric is the point")
	}
	if c.MetricsExporter != "prometheus" || c.MetricsPort != 8081 {
		t.Errorf("exporter/port = %q/%d, want prometheus/8081", c.MetricsExporter, c.MetricsPort)
	}
}

func TestMetricsSettingsRejected(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"METRICS_ENABLED", "yes please"},
		{"METRICS_EXPORTER", "statsd"},
		{"METRICS_PORT", "0"},
		{"METRICS_PORT", "70000"},
		{"METRICS_PORT", "eighty-eighty"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			setRequired(t)
			t.Setenv(tc.key, tc.val)

			if _, err := Load(); err == nil {
				t.Fatalf("%s=%q was accepted", tc.key, tc.val)
			} else if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error does not name %s:\n%v", tc.key, err)
			}
		})
	}
}

// A bad port must not fail the build when metrics are switched off entirely.
func TestMetricsPortIgnoredWhenDisabled(t *testing.T) {
	setRequired(t)
	t.Setenv("METRICS_ENABLED", "false")
	t.Setenv("METRICS_PORT", "0")

	if _, err := Load(); err != nil {
		t.Errorf("port should not be validated with metrics off: %v", err)
	}
}

func TestOutOfRangeValuesRejected(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"JPEG_QUALITY", "0"},
		{"JPEG_QUALITY", "101"},
		{"RETENTION_DAYS", "-1"},
		{"POLL_INTERVAL", "0s"}, // would panic time.NewTicker
		{"POLL_INTERVAL", "-5s"},
		{"MAX_FRAME_AGE", "0s"},
		{"STALE_AFTER", "0s"}, // would disable the staleness gate
		{"MIN_FRAME_GAP", "-1s"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			setRequired(t)
			t.Setenv(tc.key, tc.val)

			if _, err := Load(); err == nil {
				t.Fatalf("%s=%q was accepted", tc.key, tc.val)
			} else if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error does not name %s:\n%v", tc.key, err)
			}
		})
	}
}

// STALE_AFTER of zero would switch off the gate that keeps a stale reading off
// the frame, so the daemon must refuse it even though frame.Build treats zero as
// "no gating" for the harness's benefit.
func TestZeroStaleAfterRefused(t *testing.T) {
	setRequired(t)
	t.Setenv("STALE_AFTER", "0s")

	_, err := Load()
	if err == nil {
		t.Fatal("STALE_AFTER=0s was accepted; the staleness gate would be off")
	}
}

// Several problems at once should all appear, so one restart fixes everything.
func TestProblemsAreAccumulated(t *testing.T) {
	t.Setenv("PROTECT_HOST", "h")
	t.Setenv("JPEG_QUALITY", "nope")
	t.Setenv("SITE_ELEVATION_M", "1,200")

	_, err := Load()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{
		"PROTECT_API_KEY", "INFLUX_ORG", "INFLUX_TOKEN", "JPEG_QUALITY", "SITE_ELEVATION_M",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error omits %s:\n%v", want, err)
		}
	}

	// The problems stay addressable as a list, so a structured log handler can
	// emit them as an array instead of one sentence with separators in it.
	var cfgErr *Error
	if !errors.As(err, &cfgErr) {
		t.Fatalf("error is %T, want *config.Error", err)
	}
	if len(cfgErr.Problems) < 5 {
		t.Errorf("got %d problems, want at least 5: %v", len(cfgErr.Problems), cfgErr.Problems)
	}
}

// Load must keep returning an inspectable *config.Error, since main branches on
// it to decide how to log.
func TestErrorTypeIsInspectable(t *testing.T) {
	setRequired(t)
	t.Setenv("JPEG_QUALITY", "nope")

	_, err := Load()
	var cfgErr *Error
	if !errors.As(err, &cfgErr) {
		t.Fatalf("error is %T, want *config.Error", err)
	}
	if len(cfgErr.Problems) != 1 {
		t.Errorf("got %d problems, want 1: %v", len(cfgErr.Problems), cfgErr.Problems)
	}
	if !strings.HasPrefix(cfgErr.Error(), "invalid configuration: ") {
		t.Errorf("message = %q, want it to lead with the summary", cfgErr.Error())
	}
	// Single-line, so a text handler does not turn it into escaped litter.
	if strings.Contains(cfgErr.Error(), "\n") {
		t.Errorf("message contains a newline: %q", cfgErr.Error())
	}
}
