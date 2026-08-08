package config

import (
	"fmt"
	"os"
	"strconv"
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
	NWSUserAgent string // required by api.weather.gov: "towercam (you@example.com)"

	// Site
	SiteName  string
	Elevation float64 // meters, for sea-level pressure reduction
	Timezone  string

	// Rendering
	FontPath      string
	BoldFontPath  string
	LogoPath      string
	ThemePath     string
	JPEGQuality   int
	OutputDir     string
	ArchiveDir    string // empty disables archiving
	RetentionDays int

	// Timing
	PollInterval time.Duration // how often to ask Influx for a newer observation
	MinFrameGap  time.Duration // floor between rendered frames
	MaxFrameAge  time.Duration // force a frame even with no new data
	StaleAfter   time.Duration // observations older than this are not rendered
	NWSInterval  time.Duration
}

func Load() (*Config, error) {
	c := &Config{
		ProtectHost:     env("PROTECT_HOST", ""),
		ProtectAPIKey:   env("PROTECT_API_KEY", ""),
		ProtectCameraID: env("PROTECT_CAMERA_ID", ""),
		ProtectCertSHA:  env("PROTECT_CERT_SHA256", ""),

		InfluxURL:     env("INFLUX_URL", "http://localhost:8086"),
		InfluxOrg:     env("INFLUX_ORG", ""),
		InfluxToken:   env("INFLUX_TOKEN", ""),
		InfluxBucket:  env("INFLUX_BUCKET", "weather"),
		InfluxStation: env("INFLUX_STATION", ""),

		NWSStationID: env("NWS_STATION_ID", ""),
		NWSUserAgent: env("NWS_USER_AGENT", "towercam"),

		SiteName:  env("SITE_NAME", ""),
		Elevation: envFloat("SITE_ELEVATION_M", 0),
		Timezone:  env("TZ", "America/Los_Angeles"),

		FontPath:      env("FONT_PATH", "assets/font.ttf"),
		BoldFontPath:  env("BOLD_FONT_PATH", "assets/font-bold.ttf"),
		LogoPath:      env("LOGO_PATH", "assets/logo.png"),
		ThemePath:     env("THEME_PATH", ""),
		JPEGQuality:   envInt("JPEG_QUALITY", 92),
		OutputDir:     env("OUTPUT_DIR", "/var/www/towercam"),
		ArchiveDir:    env("ARCHIVE_DIR", ""),
		RetentionDays: envInt("RETENTION_DAYS", 0),

		PollInterval: envDur("POLL_INTERVAL", 15*time.Second),
		MinFrameGap:  envDur("MIN_FRAME_GAP", 55*time.Second),
		MaxFrameAge:  envDur("MAX_FRAME_AGE", 3*time.Minute),
		StaleAfter:   envDur("STALE_AFTER", 10*time.Minute),
		NWSInterval:  envDur("NWS_INTERVAL", 10*time.Minute),
	}

	for k, v := range map[string]string{
		"PROTECT_HOST":      c.ProtectHost,
		"PROTECT_API_KEY":   c.ProtectAPIKey,
		"PROTECT_CAMERA_ID": c.ProtectCameraID,
		"INFLUX_ORG":        c.InfluxOrg,
		"INFLUX_TOKEN":      c.InfluxToken,
	} {
		if v == "" {
			return nil, fmt.Errorf("config: %s is required", k)
		}
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return v
	}
	return def
}
