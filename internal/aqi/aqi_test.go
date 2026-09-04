package aqi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const indexBody = `{"generated_at":"2026-09-04T19:47:13Z","data":{"time":"2026-09-04T19:47:09.799229Z","aqi":42,"level":"Good","primary_pollutant":"PM2.5"}}`
const pmBody = `{"generated_at":"2026-09-04T19:47:13Z","data":{"time":"2026-09-04T19:47:09.789024Z","last":9.4}}`

// testClient points a client at a local server standing in for the bucket.
func testClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL + "/aqi/v1/")
}

func bucket(index, pm string, indexStatus int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/aqi/v1/aqi/last.json":
			w.WriteHeader(indexStatus)
			_, _ = w.Write([]byte(index))
		case "/aqi/v1/pm25/last.json":
			if pm == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(pm))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestFetchParsesIndexAndPM25(t *testing.T) {
	c := testClient(t, bucket(indexBody, pmBody, http.StatusOK))

	r, err := c.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if r.AQI != 42 || r.Level != "Good" || r.PrimaryPollutant != "PM2.5" {
		t.Errorf("reading = %+v", r)
	}
	if !r.HasPM25 || r.PM25 != 9.4 {
		t.Errorf("PM25 = %v (has=%v), want 9.4", r.PM25, r.HasPM25)
	}
	want := time.Date(2026, 9, 4, 19, 47, 9, 799229000, time.UTC)
	if !r.ObservedAt.Equal(want) {
		t.Errorf("ObservedAt = %s, want %s", r.ObservedAt, want)
	}
}

// The trailing slash on the configured prefix must not double up in the URL.
func TestPrefixIsNormalised(t *testing.T) {
	var paths []string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	_, _ = c.fetch(context.Background())
	if len(paths) == 0 || paths[0] != "/aqi/v1/aqi/last.json" {
		t.Errorf("requested %v, want /aqi/v1/aqi/last.json", paths)
	}
}

func TestMissingPM25IsNotAnError(t *testing.T) {
	c := testClient(t, bucket(indexBody, "", http.StatusOK))

	r, err := c.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if r.HasPM25 {
		t.Error("HasPM25 = true with no pm25 object")
	}
	if r.AQI != 42 {
		t.Errorf("AQI = %d, want 42", r.AQI)
	}
}

func TestIndexErrorsSurface(t *testing.T) {
	c := testClient(t, bucket("nope", pmBody, http.StatusServiceUnavailable))

	_, err := c.fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v, want a 503", err)
	}
}

func TestEmptyLevelRejected(t *testing.T) {
	c := testClient(t, bucket(`{"data":{"time":"2026-09-04T19:47:09Z","aqi":0}}`, pmBody, http.StatusOK))
	if _, err := c.fetch(context.Background()); err == nil {
		t.Error("expected an error for an index with no level")
	}
}

// An unset prefix disables the field: Run returns at once and Latest is nil.
func TestEmptyPrefixIsANoOp(t *testing.T) {
	c := New("")
	done := make(chan struct{})
	go func() {
		c.Run(context.Background(), time.Second, func(err error) { t.Errorf("onErr called: %v", err) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return for an empty prefix")
	}
	if c.Latest() != nil {
		t.Error("Latest() != nil for an empty prefix")
	}
	var nilClient *Client
	if nilClient.Latest() != nil {
		t.Error("nil client Latest() != nil")
	}
}

func TestRunKeepsPreviousReadingOnFailure(t *testing.T) {
	var fail atomic.Bool
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		bucket(indexBody, pmBody, http.StatusOK).ServeHTTP(w, r)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 8)
	go c.Run(ctx, 20*time.Millisecond, func(err error) { errs <- err })

	deadline := time.After(2 * time.Second)
	for c.Latest() == nil {
		select {
		case <-deadline:
			t.Fatal("no reading fetched")
		case <-time.After(5 * time.Millisecond):
		}
	}
	fail.Store(true)
	select {
	case <-errs:
	case <-deadline:
		t.Fatal("no error reported after the bucket started failing")
	}
	if c.Latest() == nil || c.Latest().AQI != 42 {
		t.Error("previous reading was not kept through a failed refresh")
	}
}

func TestShortLevel(t *testing.T) {
	cases := map[string]string{
		"Good":                           "Good",
		"Moderate":                       "Moderate",
		"Unhealthy for Sensitive Groups": "Sensitive",
		"Unhealthy":                      "Unhealthy",
		"Very Unhealthy":                 "V. Unhealthy",
		"Hazardous":                      "Hazardous",
	}
	for in, want := range cases {
		if got := ShortLevel(in); got != want {
			t.Errorf("ShortLevel(%q) = %q, want %q", in, got, want)
		}
	}
}
