// Package nws reads the plain-language conditions text from api.weather.gov.
//
// The station's own sensors give numbers; they can't say "Thunderstorm in
// Vicinity". That phrase comes from the nearest NWS reporting station, refreshed
// on its own slow schedule because it changes on the order of an hour.
package nws

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

// Observation is the conditions text and when it was taken.
type Observation struct {
	Text       string
	ObservedAt time.Time
}

// Client polls one NWS station. The zero value is not usable; call New.
//
// A nil or empty station ID makes every method a no-op, which is how the
// conditions line is disabled.
type Client struct {
	stationID string
	userAgent string
	http      *http.Client

	// baseURL is empty in production and points at the real service. Tests set
	// it to a local server; nothing else should.
	baseURL string

	mu     sync.RWMutex
	latest *Observation
}

// defaultBaseURL is where observations actually come from.
const defaultBaseURL = "https://api.weather.gov"

func (c *Client) endpoint() string {
	base := c.baseURL
	if base == "" {
		base = defaultBaseURL
	}
	return fmt.Sprintf("%s/stations/%s/observations/latest", base, c.stationID)
}

func New(stationID, userAgent string) *Client {
	if userAgent == "" {
		userAgent = "rattlecam"
	}
	return &Client{
		stationID: strings.TrimSpace(stationID),
		userAgent: userAgent,
		http:      &http.Client{Timeout: 20 * time.Second},
	}
}

// Latest returns the most recent observation, or nil if none has been fetched.
// Callers decide how old is too old — the daemon drops the line past 90 minutes.
func (c *Client) Latest() *Observation {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latest
}

// Run refreshes in the background until ctx is done, reporting failures through
// onErr. A failed refresh keeps the previous observation rather than blanking
// the line: api.weather.gov is frequently briefly unavailable, and conditions
// that are an hour stale still beat no conditions at all.
func (c *Client) Run(ctx context.Context, interval time.Duration, onErr func(error)) {
	if c == nil || c.stationID == "" {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Minute
	}

	refresh := func() {
		obs, err := c.fetch(ctx)
		if err != nil {
			if onErr != nil && ctx.Err() == nil {
				onErr(err)
			}
			return
		}
		c.mu.Lock()
		c.latest = obs
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

// apiResponse is the subset of the GeoJSON observation we care about.
type apiResponse struct {
	Properties struct {
		Timestamp       string `json:"timestamp"`
		TextDescription string `json:"textDescription"`
	} `json:"properties"`
}

func (c *Client) fetch(ctx context.Context) (*Observation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(), nil)
	if err != nil {
		return nil, fmt.Errorf("nws: build request: %w", err)
	}
	// api.weather.gov rejects requests that don't identify themselves.
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/geo+json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("nws: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return nil, fmt.Errorf("nws: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var out apiResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("nws: decode: %w", err)
	}

	text := strings.TrimSpace(out.Properties.TextDescription)
	if text == "" {
		return nil, fmt.Errorf("nws: station %s reported no conditions text", c.stationID)
	}

	ts, err := time.Parse(time.RFC3339, out.Properties.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("nws: timestamp %q: %w", out.Properties.Timestamp, err)
	}

	return &Observation{Text: text, ObservedAt: ts}, nil
}
