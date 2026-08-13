package main

import (
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

func TestNewLoggerAcceptsValidSettings(t *testing.T) {
	for _, tc := range []struct{ level, format string }{
		{"debug", "text"},
		{"INFO", "json"},
		{"Warn", ""},
		{"error", "JSON"},
		{"", "text"},
		{"  info  ", "  json  "}, // padding from a compose file
	} {
		t.Run(tc.level+"/"+tc.format, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", tc.level)
			t.Setenv("LOG_FORMAT", tc.format)

			if _, err := newLogger(); err != nil {
				t.Errorf("LOG_LEVEL=%q LOG_FORMAT=%q: %v", tc.level, tc.format, err)
			}
		})
	}
}

// A logger that cannot be built is the one failure that cannot be logged, so it
// has to be reported clearly enough for stderr to be useful.
func TestNewLoggerRejectsBadSettings(t *testing.T) {
	for _, tc := range []struct{ key, val, want string }{
		{"LOG_LEVEL", "verbose", "LOG_LEVEL"},
		{"LOG_LEVEL", "9", "LOG_LEVEL"},
		{"LOG_FORMAT", "logfmt", "LOG_FORMAT"},
		{"LOG_FORMAT", "xml", "LOG_FORMAT"},
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
