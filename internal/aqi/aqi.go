// Package aqi reads the air quality index published by aqi-api.
//
// The monitor's readings and the EPA index computed from them land in a
// public bucket every five minutes as small JSON envelopes. Reading those
// rather than the API itself means the daemon needs no route into the
// cluster and no credential: the same bytes the website reads.
// See https://github.com/michaelpeterswa/aqi-api
package aqi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Reading is the index at one moment and the PM2.5 concentration behind it.
type Reading struct {
	ObservedAt       time.Time
	AQI              int
	Level            string // EPA category, e.g. "Good"
	PrimaryPollutant string // "PM2.5" or "PM10.0"
	PM25             float64
	HasPM25          bool
}

// Client polls the published envelopes. The zero value is not usable; call
// New. A nil client or an empty base URL makes every method a no-op, which is
// how the air quality field is disabled.
type Client struct {
	baseURL string
	http    *http.Client

	mu     sync.RWMutex
	latest *Reading
}

// New takes the prefix under which aqi-api publishes, e.g.
// https://storage.googleapis.com/<bucket>/aqi/v1 — the objects read are
// <prefix>/aqi/last.json and <prefix>/pm25/last.json.
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:    &http.Client{Timeout: 20 * time.Second},
	}
}

// Latest returns the most recent reading, or nil if none has been fetched.
// Callers decide how old is too old; the daemon drops the field past thirty
// minutes, six publish cycles.
func (c *Client) Latest() *Reading {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latest
}

// Run refreshes in the background until ctx is done, reporting failures
// through onErr. A failed refresh keeps the previous reading rather than
// blanking the field; the age gate at the point of use bounds how long that
// can go on.
func (c *Client) Run(ctx context.Context, interval time.Duration, onErr func(error)) {
	if c == nil || c.baseURL == "" {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Minute
	}

	refresh := func() {
		r, err := c.fetch(ctx)
		if err != nil {
			if onErr != nil && ctx.Err() == nil {
				onErr(err)
			}
			return
		}
		c.mu.Lock()
		c.latest = r
		c.mu.Unlock()
	}

	refresh()

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			refresh()
		}
	}
}

// envelope is how aqi-api wraps every per-endpoint object.
type envelope[T any] struct {
	GeneratedAt string `json:"generated_at"`
	Data        T      `json:"data"`
}

type aqiLast struct {
	Time             string `json:"time"`
	AQI              int    `json:"aqi"`
	Level            string `json:"level"`
	PrimaryPollutant string `json:"primary_pollutant"`
}

type metricLast struct {
	Time string  `json:"time"`
	Last float64 `json:"last"`
}

// fetch reads the index and, best effort, the PM2.5 behind it. The index is
// the field; a missing PM2.5 object is not an error.
func (c *Client) fetch(ctx context.Context) (*Reading, error) {
	var index envelope[aqiLast]
	if err := c.get(ctx, c.baseURL+"/aqi/last.json", &index); err != nil {
		return nil, err
	}
	if index.Data.Level == "" {
		return nil, fmt.Errorf("aqi: index object carries no level")
	}
	observed, err := time.Parse(time.RFC3339Nano, index.Data.Time)
	if err != nil {
		return nil, fmt.Errorf("aqi: time %q: %w", index.Data.Time, err)
	}

	r := &Reading{
		ObservedAt:       observed,
		AQI:              index.Data.AQI,
		Level:            index.Data.Level,
		PrimaryPollutant: index.Data.PrimaryPollutant,
	}

	var pm envelope[metricLast]
	if err := c.get(ctx, c.baseURL+"/pm25/last.json", &pm); err == nil {
		r.PM25 = pm.Data.Last
		r.HasPM25 = true
	}
	return r, nil
}

func (c *Client) get(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("aqi: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("aqi: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return fmt.Errorf("aqi: %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("aqi: decode %s: %w", url, err)
	}
	return nil
}

// ShortLevel compresses the EPA category names to what fits in a data column.
// "Unhealthy for Sensitive Groups" is wider than the whole strip allows.
func ShortLevel(level string) string {
	switch level {
	case "Unhealthy for Sensitive Groups":
		return "Sensitive"
	case "Very Unhealthy":
		return "V. Unhealthy"
	default:
		return level
	}
}
