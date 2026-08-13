package spool

import (
	"os"
	"path/filepath"
	"testing"
)

func open(t *testing.T, maxBytes int64) *Spool {
	t.Helper()
	s, err := Open(t.TempDir(), maxBytes)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func objects(t *testing.T, s *Spool) []string {
	t.Helper()
	entries, err := s.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Object
	}
	return out
}

// The whole point: a frame rendered during an outage is still there afterwards.
func TestEntriesSurviveAndCanBeRead(t *testing.T) {
	s := open(t, 0)

	if err := s.Add(Latest, "latest.jpg", []byte("frame")); err != nil {
		t.Fatalf("Add: %v", err)
	}

	entries, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	data, err := entries[0].Read()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "frame" {
		t.Errorf("data = %q, want %q", data, "frame")
	}
}

// A restart mid-outage must not lose the backlog, since the outages worth
// surviving outlast the process.
func TestSpoolSurvivesReopen(t *testing.T) {
	dir := t.TempDir()

	s, err := Open(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Archive, "archive/2026/08/13/120000.jpg", []byte("a")); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := objects(t, reopened); len(got) != 1 || got[0] != "archive/2026/08/13/120000.jpg" {
		t.Errorf("after reopen: %v", got)
	}
}

// Draining a backlog of superseded "latest" frames in order would walk the
// public image backwards through the outage before catching up. Only the newest
// may survive.
func TestLatestCoalesces(t *testing.T) {
	s := open(t, 0)

	for _, body := range []string{"old", "newer", "newest"} {
		if err := s.Add(Latest, "latest.jpg", []byte(body)); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := s.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d pending latest frames, want 1: %v", len(entries), objects(t, s))
	}
	data, _ := entries[0].Read()
	if string(data) != "newest" {
		t.Errorf("kept %q, want the newest frame", data)
	}
}

// Both latest objects are distinct names and must not collapse into each other.
func TestLatestKeepsDistinctNamesApart(t *testing.T) {
	s := open(t, 0)

	if err := s.Add(Latest, "latest.jpg", []byte("branded")); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Latest, "latest-clean.jpg", []byte("clean")); err != nil {
		t.Fatal(err)
	}
	if got := objects(t, s); len(got) != 2 {
		t.Errorf("got %v, want both latest objects pending", got)
	}
}

// Every archived moment matters; a timelapse with holes is the thing this
// exists to prevent.
func TestArchiveKeepsEveryEntry(t *testing.T) {
	s := open(t, 0)

	names := []string{
		"archive/2026/08/13/120000.jpg",
		"archive/2026/08/13/120100.jpg",
		"archive/2026/08/13/120200.jpg",
	}
	for _, n := range names {
		if err := s.Add(Archive, n, []byte(n)); err != nil {
			t.Fatal(err)
		}
	}

	got := objects(t, s)
	if len(got) != len(names) {
		t.Fatalf("got %d entries, want %d: %v", len(got), len(names), got)
	}
	// Archive names carry the timestamp, so lexical order is chronological and
	// a drain replays the outage in the order it happened.
	for i, n := range names {
		if got[i] != n {
			t.Errorf("entry %d = %s, want %s (chronological)", i, got[i], n)
		}
	}
}

// The current image should be restored before history is backfilled.
func TestLatestDrainsBeforeArchive(t *testing.T) {
	s := open(t, 0)

	if err := s.Add(Archive, "archive/2026/08/13/120000.jpg", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(Latest, "latest.jpg", []byte("l")); err != nil {
		t.Fatal(err)
	}

	got := objects(t, s)
	if len(got) != 2 || got[0] != "latest.jpg" {
		t.Errorf("order = %v, want latest.jpg first", got)
	}
}

func TestDoneRemovesEntry(t *testing.T) {
	s := open(t, 0)
	if err := s.Add(Latest, "latest.jpg", []byte("x")); err != nil {
		t.Fatal(err)
	}

	entries, _ := s.Pending()
	if err := s.Done(entries[0]); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if got := objects(t, s); len(got) != 0 {
		t.Errorf("still pending after Done: %v", got)
	}

	// Removing twice is not an error; a retry that raced a success must not
	// bring the daemon down.
	if err := s.Done(entries[0]); err != nil {
		t.Errorf("second Done: %v", err)
	}
}

// A tower disk fills in under a day at a frame a minute. Wedging on a full disk
// would turn a network outage into a total one.
func TestEvictsOldestArchiveWhenFull(t *testing.T) {
	s := open(t, 300)

	for _, n := range []string{
		"archive/2026/08/13/120000.jpg",
		"archive/2026/08/13/120100.jpg",
		"archive/2026/08/13/120200.jpg",
		"archive/2026/08/13/120300.jpg",
	} {
		if err := s.Add(Archive, n, make([]byte, 100)); err != nil {
			t.Fatalf("Add %s: %v", n, err)
		}
	}

	got := objects(t, s)
	if len(got) == 0 {
		t.Fatal("everything was evicted")
	}
	// The oldest goes first, so what remains is the most recent history.
	if got[0] == "archive/2026/08/13/120000.jpg" {
		t.Errorf("kept the oldest entry and dropped newer ones: %v", got)
	}

	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Bytes > 300 {
		t.Errorf("spool holds %d bytes, over the 300 cap", st.Bytes)
	}
}

// Evicting the current frame to make room for history would be exactly
// backwards: it is one small file and the only one viewers see.
func TestLatestIsNeverEvicted(t *testing.T) {
	s := open(t, 250)

	if err := s.Add(Latest, "latest.jpg", make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{
		"archive/2026/08/13/120000.jpg",
		"archive/2026/08/13/120100.jpg",
		"archive/2026/08/13/120200.jpg",
	} {
		if err := s.Add(Archive, n, make([]byte, 100)); err != nil {
			t.Fatal(err)
		}
	}

	var sawLatest bool
	for _, o := range objects(t, s) {
		if o == "latest.jpg" {
			sawLatest = true
		}
	}
	if !sawLatest {
		t.Errorf("latest.jpg was evicted: %v", objects(t, s))
	}
}

func TestStats(t *testing.T) {
	s := open(t, 0)
	if err := s.Add(Archive, "archive/2026/08/13/120000.jpg", make([]byte, 512)); err != nil {
		t.Fatal(err)
	}

	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Entries != 1 || st.Bytes != 512 {
		t.Errorf("stats = %+v, want 1 entry of 512 bytes", st)
	}
	if st.Oldest.IsZero() {
		t.Error("Oldest is zero")
	}
}

// A crash mid-write must not leave a truncated frame that later uploads as a
// corrupt object.
func TestPartialWritesAreNotVisible(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, 0)
	if err != nil {
		t.Fatal(err)
	}

	stray := filepath.Join(dir, Archive.dir(), ".tmp-halfwritten")
	if err := os.WriteFile(stray, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := objects(t, s); len(got) != 0 {
		t.Errorf("a temp file was offered for upload: %v", got)
	}
}

func TestOpenRejectsEmptyDir(t *testing.T) {
	if _, err := Open("", 0); err == nil {
		t.Fatal("want an error for an empty directory")
	}
}
