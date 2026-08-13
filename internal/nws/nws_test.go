package nws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const validBody = `{
  "properties": {
    "timestamp": "2026-08-12T18:53:00+00:00",
    "textDescription": "Partly Cloudy"
  }
}`

// testClient points a client at a local server standing in for api.weather.gov.
func testClient(t *testing.T, h http.Handler) *Client {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c := New("KPAE", "rattlecam (test)")
	c.baseURL = srv.URL
	return c
}

func handlerFor(status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

func TestFetchParsesObservation(t *testing.T) {
	c := testClient(t, handlerFor(http.StatusOK, validBody))

	obs, err := c.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if obs.Text != "Partly Cloudy" {
		t.Errorf("Text = %q, want %q", obs.Text, "Partly Cloudy")
	}
	want := time.Date(2026, 8, 12, 18, 53, 0, 0, time.UTC)
	if !obs.ObservedAt.Equal(want) {
		t.Errorf("ObservedAt = %s, want %s", obs.ObservedAt, want)
	}
}

// api.weather.gov rejects anonymous requests, so the identification has to
// actually go out on the wire.
func TestFetchIdentifiesItself(t *testing.T) {
	var gotAgent, gotAccept, gotPath string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAgent = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(validBody))
	}))

	if _, err := c.fetch(context.Background()); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gotAgent != "rattlecam (test)" {
		t.Errorf("User-Agent = %q, want %q", gotAgent, "rattlecam (test)")
	}
	if gotAccept != "application/geo+json" {
		t.Errorf("Accept = %q, want application/geo+json", gotAccept)
	}
	if want := "/stations/KPAE/observations/latest"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestNewDefaultsUserAgent(t *testing.T) {
	if got := New("KPAE", "").userAgent; got != "rattlecam" {
		t.Errorf("userAgent = %q, want rattlecam", got)
	}
}

func TestEndpointDefaultsToTheRealService(t *testing.T) {
	got := New("KPAE", "ua").endpoint()
	if want := defaultBaseURL + "/stations/KPAE/observations/latest"; got != want {
		t.Errorf("endpoint = %q, want %q", got, want)
	}
}

func TestFetchErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"non-200", http.StatusServiceUnavailable, "upstream down", "503"},
		{"malformed json", http.StatusOK, "{not json", "decode"},
		{"no conditions text", http.StatusOK,
			`{"properties":{"timestamp":"2026-08-12T18:53:00+00:00","textDescription":"  "}}`,
			"no conditions text"},
		{"bad timestamp", http.StatusOK,
			`{"properties":{"timestamp":"yesterday","textDescription":"Clear"}}`,
			"timestamp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, handlerFor(tc.status, tc.body))

			_, err := c.fetch(context.Background())
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestLatestIsNilBeforeAnyFetch(t *testing.T) {
	if obs := New("KPAE", "ua").Latest(); obs != nil {
		t.Errorf("Latest = %+v, want nil", obs)
	}
	var nilClient *Client
	if obs := nilClient.Latest(); obs != nil {
		t.Errorf("Latest on a nil client = %+v, want nil", obs)
	}
}

func TestRunPopulatesLatest(t *testing.T) {
	c := testClient(t, handlerFor(http.StatusOK, validBody))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, time.Hour, func(err error) { t.Errorf("unexpected error: %v", err) })
	}()

	waitFor(t, func() bool { return c.Latest() != nil })

	if got := c.Latest().Text; got != "Partly Cloudy" {
		t.Errorf("Text = %q, want %q", got, "Partly Cloudy")
	}

	cancel()
	<-done
}

// A failed refresh keeps the previous observation. Conditions an hour stale
// still beat a blank line, and the upstream is frequently briefly unavailable.
func TestRunKeepsPreviousObservationOnFailure(t *testing.T) {
	var fail atomic.Bool
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(validBody))
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var errCount atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, 10*time.Millisecond, func(error) { errCount.Add(1) })
	}()

	waitFor(t, func() bool { return c.Latest() != nil })
	first := c.Latest()

	fail.Store(true)
	waitFor(t, func() bool { return errCount.Load() > 0 })

	if got := c.Latest(); got == nil {
		t.Fatal("a failed refresh blanked the observation")
	} else if got.Text != first.Text {
		t.Errorf("Text = %q, want the retained %q", got.Text, first.Text)
	}

	cancel()
	<-done
}

// An empty station ID is how the conditions line is switched off, so Run must
// return immediately rather than polling a nonsense URL forever.
func TestRunIsANoOpWithoutAStation(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
	}))
	defer srv.Close()

	c := New("", "ua")
	c.baseURL = srv.URL

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(context.Background(), time.Millisecond, func(error) {})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return for an empty station ID")
	}
	if called.Load() {
		t.Error("Run made a request despite having no station")
	}

	var nilClient *Client
	nilClient.Run(context.Background(), time.Millisecond, nil) // must not panic
}

// Cancelling the context stops the loop, and the shutdown itself must not be
// reported as a refresh failure.
func TestRunStopsOnContextCancel(t *testing.T) {
	c := testClient(t, handlerFor(http.StatusOK, validBody))

	ctx, cancel := context.WithCancel(context.Background())
	var errCount atomic.Int64

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, time.Millisecond, func(error) { errCount.Add(1) })
	}()

	waitFor(t, func() bool { return c.Latest() != nil })
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after the context was cancelled")
	}
	if n := errCount.Load(); n != 0 {
		t.Errorf("shutdown reported %d refresh errors, want 0", n)
	}
}

// A non-positive interval must not reach time.NewTicker, which panics on one.
func TestRunRejectsNonPositiveInterval(t *testing.T) {
	c := testClient(t, handlerFor(http.StatusOK, validBody))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, 0, func(error) {}) // would panic if passed straight through
	}()

	waitFor(t, func() bool { return c.Latest() != nil })
	cancel()
	<-done
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within the deadline")
}
