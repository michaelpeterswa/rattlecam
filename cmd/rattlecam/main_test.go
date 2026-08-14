package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestNewLoggerDefaults(t *testing.T) {
	log, err := newLogger()
	if err != nil {
		t.Fatalf("newLogger: %v", err)
	}
	if log == nil {
		t.Fatal("newLogger returned a nil logger")
	}
}

func TestNewLoggerAcceptsValidLevels(t *testing.T) {
	for _, level := range []string{"debug", "INFO", "Warn", "error", "", "  info  "} {
		t.Run(level, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", level)

			if _, err := newLogger(); err != nil {
				t.Errorf("LOG_LEVEL=%q: %v", level, err)
			}
		})
	}
}

// The output is JSON and there is no setting that makes it anything else, so a
// collector can be pointed at it without conditions.
func TestNewLoggerEmitsJSON(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	log, err := newLogger()
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hello", "luma", 41.6)
	w.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, buf.String())
	}
	if got["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", got["msg"])
	}
	if got["luma"] != 41.6 {
		t.Errorf("luma = %v, want 41.6 — structured fields must survive as fields", got["luma"])
	}
}

// It must also be installed as the default, or anything using the package-level
// functions writes plain text into the same stream.
func TestNewLoggerBecomesTheDefault(t *testing.T) {
	log, err := newLogger()
	if err != nil {
		t.Fatal(err)
	}
	if slog.Default() != log {
		t.Error("newLogger did not install itself as slog's default")
	}
}

// A logger that cannot be built is the one failure that cannot be logged, so it
// has to be reported clearly enough for stderr to be useful.
func TestNewLoggerRejectsBadSettings(t *testing.T) {
	for _, tc := range []struct{ key, val, want string }{
		{"LOG_LEVEL", "verbose", "LOG_LEVEL"},
		{"LOG_LEVEL", "9", "LOG_LEVEL"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)

			log, err := newLogger()
			if err == nil {
				t.Fatalf("%s=%q was accepted", tc.key, tc.val)
			}
			if log != nil {
				t.Error("a failed newLogger still returned a logger")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.val) {
				t.Errorf("error %q does not quote the offending value", err)
			}
		})
	}
}
