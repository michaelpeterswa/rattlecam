package config

import (
	"fmt"
	"math"
	"os"

	"github.com/michaelpeterswa/rattlecam/internal/light"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// UniFi Protect
	ProtectHost     string // e.g. 192.168.1.1 (console address, no scheme)
	ProtectAPIKey   string
	ProtectCameraID string
	ProtectCertSHA  string // hex sha256 of the console's leaf cert; empty = skip verification

	// InfluxDB v2
	InfluxURL     string // e.g. https://metrics.example.com
	InfluxOrg     string
	InfluxToken   string
	InfluxBucket  string
	InfluxStation string // Tempest serial; empty = don't filter by station

	// NWS
	NWSStationID string // e.g. KPAE. Empty disables the conditions text.
	NWSUserAgent string // required by api.weather.gov: "rattlecam (you@example.com)"

	// Site
	SiteName  string
	Credit    string  // standing attribution line, empty hides it
	Elevation float64 // meters, for sea-level pressure reduction
	Timezone  string

	// Rendering
	FontPath       string
	BoldFontPath   string
	LogoPath       string
	AnnotationPath string
	ThemePath      string
	JPEGQuality    int

	// Night. The state is measured from the frame's own mean luma rather than a
	// clock, because what matters is whether black ink is still visible, and
	// overcast, smoke and the camera's own exposure move that hours either side
	// of sunset.
	//
	// NightEnter and NightExit are a hysteresis band on a 0-255 scale. Real
	// frames from this site measure around 10 at night and above 100 in daylight,
	// so the defaults sit in the empty middle with room on both sides.
	NightEnter   float64
	NightExit    float64
	NightInvert  bool // draw the annotation in white after dark
	NightArchive bool // keep archiving through the night

	// WebWidth publishes an extra, narrower copy of the branded frame for
	// websites. Zero disables it.
	WebWidth      int
	OutputDir     string
	ArchiveDir    string // empty disables archiving
	RetentionDays int

	// Object storage. GCSBucket empty disables it entirely and publishing stays
	// local-only.
	GCSBucket       string
	GCSPrefix       string
	GCSArchive      bool
	GCSCacheControl string

	// Frames are queued here when the link to the bucket is down, and drained
	// when it returns. Empty disables queueing and uploads become inline.
	SpoolDir      string
	SpoolMaxBytes int

	// Observability
	MetricsEnabled  bool
	MetricsExporter string // prometheus, otlpgrpc or otlphttp
	MetricsPort     int

	// Timing
	PollInterval time.Duration // how often to ask Influx for a newer observation
	MinFrameGap  time.Duration // floor between rendered frames
	MaxFrameAge  time.Duration // force a frame even with no new data
	StaleAfter   time.Duration // observations older than this are not rendered
	NWSInterval  time.Duration

	// Warnings are settings that were accepted but adjusted, or that will not
	// do what they look like they do. Not fatal, but the operator should see
	// them rather than discover the behaviour later.
	Warnings []string
}

func Load() (*Config, error) {
	l := &loader{}

	c := &Config{
		ProtectHost:     l.str("PROTECT_HOST", ""),
		ProtectAPIKey:   l.str("PROTECT_API_KEY", ""),
		ProtectCameraID: l.str("PROTECT_CAMERA_ID", ""),
		ProtectCertSHA:  l.str("PROTECT_CERT_SHA256", ""),

		InfluxURL:     l.str("INFLUX_URL", "http://localhost:8086"),
		InfluxOrg:     l.str("INFLUX_ORG", ""),
		InfluxToken:   l.str("INFLUX_TOKEN", ""),
		InfluxBucket:  l.str("INFLUX_BUCKET", "weather"),
		InfluxStation: l.str("INFLUX_STATION", ""),

		NWSStationID: l.str("NWS_STATION_ID", ""),
		NWSUserAgent: l.str("NWS_USER_AGENT", "rattlecam"),

		SiteName:  l.str("SITE_NAME", ""),
		Credit:    l.str("CREDIT", ""),
		Elevation: l.float("SITE_ELEVATION_M", 0),
		Timezone:  l.str("TZ", "America/Los_Angeles"),

		FontPath:       l.str("FONT_PATH", "assets/font.ttf"),
		BoldFontPath:   l.str("BOLD_FONT_PATH", "assets/font-bold.ttf"),
		LogoPath:       l.str("LOGO_PATH", "assets/logo.png"),
		AnnotationPath: l.str("ANNOTATION_PATH", "assets/annotation.png"),
		ThemePath:      l.str("THEME_PATH", ""),
		JPEGQuality:    l.int("JPEG_QUALITY", 92),

		NightEnter:   l.float("NIGHT_ENTER_LUMA", light.DefaultEnter),
		NightExit:    l.float("NIGHT_EXIT_LUMA", light.DefaultExit),
		NightInvert:  l.bool("NIGHT_INVERT_ANNOTATION", true),
		NightArchive: l.bool("NIGHT_ARCHIVE", false),

		WebWidth:      l.int("WEB_WIDTH", 1280),
		OutputDir:     l.str("OUTPUT_DIR", "/var/www/rattlecam"),
		ArchiveDir:    l.str("ARCHIVE_DIR", ""),
		RetentionDays: l.int("RETENTION_DAYS", 0),

		GCSBucket:       l.str("GCS_BUCKET", ""),
		GCSPrefix:       l.str("GCS_PREFIX", ""),
		GCSArchive:      l.bool("GCS_ARCHIVE", true),
		GCSCacheControl: l.str("GCS_CACHE_CONTROL", "no-cache, max-age=0, must-revalidate"),
		SpoolDir:        l.str("SPOOL_DIR", "/var/spool/rattlecam"),
		SpoolMaxBytes:   l.int("SPOOL_MAX_BYTES", 2<<30),

		MetricsEnabled:  l.bool("METRICS_ENABLED", true),
		MetricsExporter: l.str("METRICS_EXPORTER", "prometheus"),
		MetricsPort:     l.int("METRICS_PORT", 8081),

		PollInterval: l.dur("POLL_INTERVAL", 15*time.Second),
		MinFrameGap:  l.dur("MIN_FRAME_GAP", 55*time.Second),
		MaxFrameAge:  l.dur("MAX_FRAME_AGE", 3*time.Minute),
		StaleAfter:   l.dur("STALE_AFTER", 10*time.Minute),
		NWSInterval:  l.dur("NWS_INTERVAL", 10*time.Minute),
	}

	// Ordered, so the reported list is stable between runs.
	for _, req := range []struct {
		key, val string
	}{
		{"PROTECT_HOST", c.ProtectHost},
		{"PROTECT_API_KEY", c.ProtectAPIKey},
		{"PROTECT_CAMERA_ID", c.ProtectCameraID},
		{"INFLUX_ORG", c.InfluxOrg},
		{"INFLUX_TOKEN", c.InfluxToken},
	} {
		if req.val == "" {
			l.fail("%s is required", req.key)
		}
	}

	switch c.MetricsExporter {
	case "prometheus", "otlpgrpc", "otlphttp":
	default:
		l.fail("METRICS_EXPORTER: %q is not an exporter (want prometheus, otlpgrpc or otlphttp)", c.MetricsExporter)
	}
	if c.MetricsEnabled && (c.MetricsPort < 1 || c.MetricsPort > 65535) {
		l.fail("METRICS_PORT: %d is not a port", c.MetricsPort)
	}

	if c.JPEGQuality < 1 || c.JPEGQuality > 100 {
		l.fail("JPEG_QUALITY: %d is outside 1..100", c.JPEGQuality)
	}
	if c.WebWidth < 0 || c.WebWidth > 8192 {
		l.fail("WEB_WIDTH: %d is outside 0..8192 (0 disables the web copy)", c.WebWidth)
	}
	if c.SpoolMaxBytes < 0 {
		l.fail("SPOOL_MAX_BYTES: %d is negative", c.SpoolMaxBytes)
	}
	if c.RetentionDays < 0 {
		l.fail("RETENTION_DAYS: %d is negative (0 keeps archives forever)", c.RetentionDays)
	}

	// Luma is a 0-255 mean, so a threshold outside that can never be crossed:
	// too high and every frame is night forever, too low and none ever is.
	for _, n := range []struct {
		key string
		val float64
	}{
		{"NIGHT_ENTER_LUMA", c.NightEnter},
		{"NIGHT_EXIT_LUMA", c.NightExit},
	} {
		if n.val < 0 || n.val > 255 {
			l.fail("%s: %g is outside 0..255", n.key, n.val)
		}
	}
	// Equal thresholds are a single threshold, which flaps: dusk sits on the
	// boundary and the annotation's colour flips back and forth every poll.
	// Inverted ones are worse — night would latch on and never release.
	if c.NightExit <= c.NightEnter {
		l.fail("NIGHT_EXIT_LUMA (%g) must be above NIGHT_ENTER_LUMA (%g); they are a hysteresis band, and without a gap between them the night state would flip on every poll through dusk",
			c.NightExit, c.NightEnter)
	}

	// POLL_INTERVAL drives a time.Ticker, which panics on a non-positive
	// interval, and STALE_AFTER at zero would switch off the staleness gate
	// altogether — the one thing that must never happen unattended.
	for _, d := range []struct {
		key string
		val time.Duration
	}{
		{"POLL_INTERVAL", c.PollInterval},
		{"MAX_FRAME_AGE", c.MaxFrameAge},
		{"STALE_AFTER", c.StaleAfter},
		{"NWS_INTERVAL", c.NWSInterval},
	} {
		if d.val <= 0 {
			l.fail("%s: must be positive, got %s", d.key, d.val)
		}
	}
	// A zero floor is meaningful: render as fast as observations arrive.
	if c.MinFrameGap < 0 {
		l.fail("MIN_FRAME_GAP: must not be negative, got %s", c.MinFrameGap)
	}

	// MAX_FRAME_AGE is only ever evaluated inside a poll tick, so it cannot
	// force a frame sooner than POLL_INTERVAL no matter how low it is set.
	// Left alone it would sit there looking like a three-minute guarantee while
	// quietly delivering whatever the poll interval allows, so it is clamped to
	// the value it actually behaves as.
	if c.MaxFrameAge > 0 && c.PollInterval > 0 && c.MaxFrameAge < c.PollInterval {
		l.warn("MAX_FRAME_AGE (%s) is below POLL_INTERVAL (%s); it is checked once per poll, so it cannot force a frame any sooner. Clamped to %s.",
			c.MaxFrameAge, c.PollInterval, c.PollInterval)
		c.MaxFrameAge = c.PollInterval
	}
	c.Warnings = slices.Clone(l.warnings)

	if err := l.err(); err != nil {
		return nil, err
	}
	return c, nil
}

// loader reads the environment, collecting every problem instead of stopping at
// the first. Someone bringing up a deployment wants the whole list, not one
// error per restart.
type loader struct {
	problems []string
	warnings []string
}

func (l *loader) fail(format string, args ...any) {
	l.problems = append(l.problems, fmt.Sprintf(format, args...))
}

// warn records something the operator should see but that does not stop startup.
func (l *loader) warn(format string, args ...any) {
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}

func (l *loader) err() error {
	if len(l.problems) == 0 {
		return nil
	}
	return &Error{Problems: slices.Clone(l.problems)}
}

// Error reports everything wrong with the environment at once, so one restart
// fixes the lot.
//
// Problems stays available separately from the message because this is normally
// logged: a structured handler can emit the list as a real array, where a
// message carrying embedded newlines would just come out as escaped litter.
type Error struct {
	Problems []string
}

func (e *Error) Error() string {
	return "invalid configuration: " + strings.Join(e.Problems, "; ")
}

// lookup treats unset and empty alike, so a variable blanked out in a compose
// file behaves the same as one that was never set.
func lookup(k string) (string, bool) {
	v, ok := os.LookupEnv(k)
	if !ok {
		return "", false
	}
	if v = strings.TrimSpace(v); v == "" {
		return "", false
	}
	return v, true
}

func (l *loader) str(k, def string) string {
	if v, ok := lookup(k); ok {
		return v
	}
	return def
}

// The numeric readers below all take the same line: an unset variable falls back
// to the default, but one that is set and unparseable is a mistake and says so.
// Quietly substituting the default hides real misconfiguration — a comma in
// SITE_ELEVATION_M would otherwise silently yield elevation 0, which turns the
// pressure reduction into a no-op and publishes raw station pressure.
func (l *loader) int(k string, def int) int {
	raw, ok := lookup(k)
	if !ok {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		l.fail("%s: %q is not a whole number", k, raw)
		return def
	}
	return v
}

func (l *loader) bool(k string, def bool) bool {
	raw, ok := lookup(k)
	if !ok {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		l.fail("%s: %q is not a boolean (want true or false)", k, raw)
		return def
	}
	return v
}

func (l *loader) float(k string, def float64) float64 {
	raw, ok := lookup(k)
	if !ok {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		l.fail("%s: %q is not a number (use a plain decimal, e.g. 1200 or 1200.5)", k, raw)
		return def
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		l.fail("%s: %q is not a finite number", k, raw)
		return def
	}
	return v
}

func (l *loader) dur(k string, def time.Duration) time.Duration {
	raw, ok := lookup(k)
	if !ok {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		l.fail("%s: %q is not a duration (needs a unit, e.g. 15s, 3m, 1h)", k, raw)
		return def
	}
	return v
}
