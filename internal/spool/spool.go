// Package spool holds frames on disk until the object store can take them.
//
// The camera is on a mountain and the link to it is not reliable. Without a
// spool an outage is simply lost footage: the frame is rendered, the upload
// fails, and that moment never reaches the bucket. With one, the daemon keeps
// working through the outage and catches up afterwards.
package spool

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind decides what happens to an entry that is still waiting when a newer one
// arrives. The two published object families want opposite answers.
type Kind int

const (
	// Latest keeps only the newest entry for a given name.
	//
	// After an outage a backlog of old "latest" frames is worse than useless:
	// draining it in order would march the public image backwards through the
	// last hour before finally arriving at the present. Only the newest one is
	// worth sending, so a new entry replaces the pending one.
	Latest Kind = iota

	// Archive keeps every entry.
	//
	// Each is a distinct moment under its own timestamped name, and a timelapse
	// with holes in it is the thing the spool exists to prevent.
	Archive
)

func (k Kind) dir() string {
	if k == Latest {
		return "latest"
	}
	return "archive"
}

// Spool is a bounded, durable queue of pending uploads.
//
// Entries survive a restart, because the outages worth surviving are longer
// than the daemon's uptime guarantees.
type Spool struct {
	dir      string
	maxBytes int64

	mu sync.Mutex
}

// Entry is one pending upload.
type Entry struct {
	Kind   Kind
	Object string // the object name to write in the bucket
	path   string
}

// Open prepares a spool directory. maxBytes caps the archive backlog; zero
// means the default.
func Open(dir string, maxBytes int64) (*Spool, error) {
	if dir == "" {
		return nil, errors.New("spool: directory is required")
	}
	if maxBytes <= 0 {
		maxBytes = 2 << 30 // 2 GiB, roughly half a day of frames
	}
	for _, k := range []Kind{Latest, Archive} {
		if err := os.MkdirAll(filepath.Join(dir, k.dir()), 0o755); err != nil {
			return nil, fmt.Errorf("spool: %w", err)
		}
	}
	return &Spool{dir: dir, maxBytes: maxBytes}, nil
}

// name encodes an object name into one path segment, so an object with slashes
// in it does not turn into nested directories.
func name(object string) string { return url.PathEscape(object) }

func object(name string) (string, error) {
	s, err := url.PathUnescape(name)
	if err != nil {
		return "", fmt.Errorf("spool: undecodable entry %q: %w", name, err)
	}
	return s, nil
}

// Add queues data for upload, replacing any pending entry of the same name for
// Latest and adding a new one for Archive.
func (s *Spool) Add(kind Kind, obj string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.evictLocked(int64(len(data))); err != nil {
		return err
	}

	dst := filepath.Join(s.dir, kind.dir(), name(obj))

	// Written through a temp file and renamed, so a crash mid-write cannot
	// leave a truncated frame that later uploads as a corrupt object.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return fmt.Errorf("spool: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("spool: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("spool: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("spool: close: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("spool: rename: %w", err)
	}
	return nil
}

// Pending lists queued entries, newest-superseding entries first so a drain
// restores the current image before it starts backfilling history.
func (s *Spool) Pending() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingLocked()
}

func (s *Spool) pendingLocked() ([]Entry, error) {
	var out []Entry
	for _, k := range []Kind{Latest, Archive} {
		dir := filepath.Join(s.dir, k.dir())
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("spool: %w", err)
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".tmp-") {
				continue
			}
			names = append(names, e.Name())
		}
		// Archive names carry the timestamp, so lexical order is chronological.
		sort.Strings(names)
		for _, n := range names {
			obj, err := object(n)
			if err != nil {
				continue // not ours; leave it be
			}
			out = append(out, Entry{Kind: k, Object: obj, path: filepath.Join(dir, n)})
		}
	}
	return out, nil
}

// Read returns an entry's bytes.
func (e Entry) Read() ([]byte, error) {
	data, err := os.ReadFile(e.path)
	if err != nil {
		return nil, fmt.Errorf("spool: read %s: %w", e.Object, err)
	}
	return data, nil
}

// Done removes an entry once it has been accepted by the store.
func (s *Spool) Done(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("spool: remove %s: %w", e.Object, err)
	}
	return nil
}

// Stats reports the backlog.
type Stats struct {
	Entries int
	Bytes   int64
	Oldest  time.Time
}

func (s *Spool) Stats() (Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statsLocked()
}

func (s *Spool) statsLocked() (Stats, error) {
	var st Stats
	entries, err := s.pendingLocked()
	if err != nil {
		return st, err
	}
	for _, e := range entries {
		info, err := os.Stat(e.path)
		if err != nil {
			continue
		}
		st.Entries++
		st.Bytes += info.Size()
		if st.Oldest.IsZero() || info.ModTime().Before(st.Oldest) {
			st.Oldest = info.ModTime()
		}
	}
	return st, nil
}

// evictLocked drops the oldest archive entries until incoming bytes will fit.
//
// A tower disk fills in well under a day at a frame a minute, and a daemon that
// wedges on a full disk has turned a network outage into a total outage. Losing
// the oldest history is the least bad option, and it is logged by the caller.
func (s *Spool) evictLocked(incoming int64) error {
	st, err := s.statsLocked()
	if err != nil {
		return err
	}
	if st.Bytes+incoming <= s.maxBytes {
		return nil
	}

	entries, err := s.pendingLocked()
	if err != nil {
		return err
	}
	// Oldest archive first. Latest is never evicted: it is one small file and
	// it is the thing viewers actually see.
	for _, e := range entries {
		if e.Kind != Archive {
			continue
		}
		info, err := os.Stat(e.path)
		if err != nil {
			continue
		}
		if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("spool: evict %s: %w", e.Object, err)
		}
		st.Bytes -= info.Size()
		if st.Bytes+incoming <= s.maxBytes {
			return nil
		}
	}
	return nil
}
