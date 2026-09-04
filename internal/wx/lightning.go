package wx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Lightning summarises the strikes the station heard over a trailing window.
//
// The Tempest reports strike_count per observation (about a minute) and
// strike_distance as the mean distance, in kilometres, of the strikes in that
// interval. Summing the counts gives the hour's total; the newest interval
// with a strike gives the last strike and how far off it was.
type Lightning struct {
	Window     time.Duration
	Strikes    int
	LastStrike time.Time
	DistanceKM float64 // mean distance of the strikes in the last interval that had any
	HasDist    bool
}

// DistanceMiles converts the mean distance to miles.
func (l *Lightning) DistanceMiles() float64 { return l.DistanceKM * 0.621371 }

// lightningQuery builds the Flux for every strike observation in the window.
// Both fields come back rather than an aggregate, because the distance that
// matters is the one recorded alongside the newest strike, and pairing those
// is easier in Go than in Flux.
func (s *InfluxSource) lightningQuery(window time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "from(bucket: %s)\n", fluxString(s.Bucket))
	fmt.Fprintf(&b, "  |> range(start: -%s)\n", fluxDuration(window))
	fmt.Fprintf(&b, "  |> filter(fn: (r) => r._measurement == %s)\n", fluxString(measurement))
	if s.Station != "" {
		fmt.Fprintf(&b, "  |> filter(fn: (r) => r.station == %s)\n", fluxString(s.Station))
	}
	b.WriteString(`  |> filter(fn: (r) => r._field == "strike_count" or r._field == "strike_distance")`)
	return b.String()
}

// Lightning returns the strike summary for the trailing window. No strikes is
// a summary with Strikes == 0, not an error; the window is the gate.
func (s *InfluxSource) Lightning(ctx context.Context, window time.Duration) (*Lightning, error) {
	if window <= 0 {
		window = time.Hour
	}
	endpoint := s.URL + "/api/v2/query?org=" + urlQueryEscape(s.Org)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(s.lightningQuery(window)))
	if err != nil {
		return nil, fmt.Errorf("wx: build request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+s.Token)
	req.Header.Set("Content-Type", "application/vnd.flux")
	req.Header.Set("Accept", "application/csv")

	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wx: lightning query: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("wx: influx %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	rows, err := parseRows(resp.Body)
	if err != nil {
		return nil, err
	}
	return summariseLightning(rows, window), nil
}

// summariseLightning folds strike rows into a Lightning. Rows arrive per
// field, so the distance is looked up by the timestamp of the newest strike.
func summariseLightning(rows []fluxRow, window time.Duration) *Lightning {
	l := &Lightning{Window: window}
	distance := make(map[time.Time]float64)
	for _, r := range rows {
		if r.field == "strike_distance" {
			distance[r.time] = r.value
		}
	}
	for _, r := range rows {
		if r.field != "strike_count" || r.value <= 0 {
			continue
		}
		l.Strikes += int(r.value + 0.5)
		if r.time.After(l.LastStrike) {
			l.LastStrike = r.time
			if d, ok := distance[r.time]; ok && d > 0 {
				l.DistanceKM, l.HasDist = d, true
			} else {
				l.DistanceKM, l.HasDist = 0, false
			}
		}
	}
	return l
}
