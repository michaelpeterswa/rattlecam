package wx

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrNoData means the query window closed with no rows. For a station that has
// gone silent this is the normal outcome, not a failure: the Flux range()
// window doubles as the staleness gate, so a dead station returns zero rows by
// construction and the caller simply publishes a frame with no weather on it.
var ErrNoData = errors.New("wx: no observation in window")

// measurement is what tempest-influxdb writes into.
const measurement = "weather"

// fields is an explicit allowlist, not a wildcard, and needs to stay that way.
// With RAPID_WIND enabled and no separate bucket, rapid_wind_* points land in
// this same measurement at a much higher rate and would otherwise be pulled in
// alongside the real observation.
var fields = []string{
	"temp", "humidity", "dew_point", "p",
	"wind_avg", "wind_gust", "wind_lull", "wind_direction",
	"uv", "solar_radiation", "illuminance",
	"precipitation", "precipitation_type",
	"strike_count", "strike_distance", "battery",
}

// InfluxSource pulls the most recent observation out of InfluxDB v2.
type InfluxSource struct {
	URL     string
	Org     string
	Token   string
	Bucket  string
	Station string // empty means don't filter by station
	Window  time.Duration

	HTTP *http.Client
}

// NewInfluxSource builds a source. window is both the query range and the
// staleness gate — see ErrNoData.
func NewInfluxSource(url, org, token, bucket, station string, window time.Duration) *InfluxSource {
	if bucket == "" {
		bucket = measurement
	}
	if window <= 0 {
		window = 10 * time.Minute
	}
	return &InfluxSource{
		URL:     strings.TrimRight(url, "/"),
		Org:     org,
		Token:   token,
		Bucket:  bucket,
		Station: station,
		Window:  window,
		HTTP:    &http.Client{Timeout: 20 * time.Second},
	}
}

// query builds the Flux for the latest value of each allowlisted field.
func (s *InfluxSource) query() string {
	var b strings.Builder
	fmt.Fprintf(&b, "from(bucket: %s)\n", fluxString(s.Bucket))
	fmt.Fprintf(&b, "  |> range(start: -%s)\n", fluxDuration(s.Window))
	fmt.Fprintf(&b, "  |> filter(fn: (r) => r._measurement == %s)\n", fluxString(measurement))
	if s.Station != "" {
		fmt.Fprintf(&b, "  |> filter(fn: (r) => r.station == %s)\n", fluxString(s.Station))
	}
	quoted := make([]string, len(fields))
	for i, f := range fields {
		quoted[i] = fluxString(f)
	}
	fmt.Fprintf(&b, "  |> filter(fn: (r) => contains(value: r._field, set: [%s]))\n", strings.Join(quoted, ", "))
	b.WriteString("  |> last()")
	return b.String()
}

// Latest returns the most recent observation, or ErrNoData if the station has
// been silent for the whole window.
func (s *InfluxSource) Latest(ctx context.Context) (*Reading, error) {
	endpoint := s.URL + "/api/v2/query?org=" + urlQueryEscape(s.Org)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(s.query()))
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
		return nil, fmt.Errorf("wx: query: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("wx: influx %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	return parseAnnotatedCSV(resp.Body)
}

// parseAnnotatedCSV reads Flux's annotated CSV dialect into a single Reading.
//
// The format is awkward in three specific ways, and all three are load-bearing
// here: every data row carries a leading empty column left over from the
// annotation columns; `#`-prefixed annotation lines precede each result; and a
// response may contain several tables, each of which may restate its own header
// row. Column positions therefore cannot be assumed — indices are resolved from
// whichever header row is currently in effect.
func parseAnnotatedCSV(r io.Reader) (*Reading, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1 // tables vary in width, and annotations vary again
	cr.ReuseRecord = true

	var (
		idxTime, idxField, idxValue = -1, -1, -1
		idxError, idxReference      = -1, -1
		expectHeader                bool
		out                         = make(map[string]float64, len(fields))
		latest                      time.Time
	)

	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("wx: parse csv: %w", err)
		}
		if len(rec) == 0 {
			continue
		}

		// Annotation lines (#datatype, #group, #default) introduce a table; the
		// next non-annotation line is that table's header.
		if strings.HasPrefix(rec[0], "#") {
			expectHeader = true
			continue
		}

		// A wholly empty line separates tables.
		if allEmpty(rec) {
			continue
		}

		if expectHeader || idxValue < 0 {
			if h := indexOf(rec, "_value"); h >= 0 || indexOf(rec, "error") >= 0 {
				idxTime = indexOf(rec, "_time")
				idxField = indexOf(rec, "_field")
				idxValue = h
				idxError = indexOf(rec, "error")
				idxReference = indexOf(rec, "reference")
				expectHeader = false
				continue
			}
		}

		// Flux reports query errors as a two-column CSV table rather than a
		// non-200, so a bare "no rows" and a broken query look alike until here.
		if idxError >= 0 && idxError < len(rec) && strings.TrimSpace(rec[idxError]) != "" {
			msg := strings.TrimSpace(rec[idxError])
			if idxReference >= 0 && idxReference < len(rec) && strings.TrimSpace(rec[idxReference]) != "" {
				msg += " (reference " + strings.TrimSpace(rec[idxReference]) + ")"
			}
			return nil, fmt.Errorf("wx: influx query error: %s", msg)
		}

		if idxField < 0 || idxValue < 0 || idxField >= len(rec) || idxValue >= len(rec) {
			continue
		}

		name := strings.TrimSpace(rec[idxField])
		raw := strings.TrimSpace(rec[idxValue])
		if name == "" || raw == "" {
			continue
		}

		// Every field is a float64. tempest-influxdb writes bare numbers with no
		// `i` suffix, so even precipitation_type and strike_count arrive as
		// floats and must not be decoded as ints.
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue // a non-numeric column is not an observation we can use
		}
		out[name] = v

		if idxTime >= 0 && idxTime < len(rec) {
			if ts, err := time.Parse(time.RFC3339, strings.TrimSpace(rec[idxTime])); err == nil {
				if ts.After(latest) {
					latest = ts
				}
			}
		}
	}

	if len(out) == 0 {
		return nil, ErrNoData
	}
	if latest.IsZero() {
		return nil, fmt.Errorf("wx: response carried %d fields but no usable _time", len(out))
	}
	return &Reading{ObservedAt: latest, Fields: out}, nil
}

func indexOf(rec []string, name string) int {
	for i, c := range rec {
		if strings.TrimSpace(c) == name {
			return i
		}
	}
	return -1
}

func allEmpty(rec []string) bool {
	for _, c := range rec {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

// fluxString renders a Go string as a Flux string literal.
func fluxString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// fluxDuration renders a duration Flux will accept. Go's String() emits forms
// like "1m0s" and "1.5s" that Flux's duration literal grammar rejects, so this
// reduces to whole seconds.
func fluxDuration(d time.Duration) string {
	s := int64(d / time.Second)
	if s < 1 {
		s = 1
	}
	return strconv.FormatInt(s, 10) + "s"
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
