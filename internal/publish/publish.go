// Package publish encodes frames and gets them onto disk without a reader ever
// seeing a half-written file.
package publish

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// Publisher writes the public frame, the clean frame and the archive.
type Publisher struct {
	OutputDir     string
	ArchiveDir    string // empty disables archiving
	Quality       int
	RetentionDays int // 0 keeps archives forever
}

func (p *Publisher) quality() int {
	if p.Quality <= 0 || p.Quality > 100 {
		return 92
	}
	return p.Quality
}

// Encode renders an image to JPEG bytes.
//
// Callers encode from the pristine snapshot every time. Re-encoding an already
// composited frame stacks generational JPEG artifacts, and on a feed that runs
// for months that degradation is not recoverable.
func (p *Publisher) Encode(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: p.quality()}); err != nil {
		return nil, fmt.Errorf("publish: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// WriteAtomic writes name into dir via a temp file and a rename.
//
// The rename is the whole point: a web server polling this path must see either
// the previous frame or the new one, never a truncated JPEG. The temp file is
// created in the destination directory so the rename stays within one
// filesystem, where it is atomic.
func (p *Publisher) WriteAtomic(dir, name string, data []byte) error {
	if dir == "" {
		return fmt.Errorf("publish: no directory for %s", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("publish: mkdir %s: %w", dir, err)
	}

	final := filepath.Join(dir, name)

	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return fmt.Errorf("publish: temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()

	// Any failure past this point must not leave the temp file behind, and must
	// not touch the file already being served. Cleanup is best effort: the write
	// has already failed, and the caller needs that error rather than whatever
	// went wrong tidying up after it.
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("publish: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("publish: sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("publish: close %s: %w", tmpName, err)
	}
	// CreateTemp makes the file 0600; a web server needs to read it.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("publish: chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("publish: rename to %s: %w", final, err)
	}
	return nil
}

// ArchivePath is where the master for a given instant lives, relative to
// ArchiveDir.
func ArchivePath(t time.Time) (dir, name string) {
	return filepath.Join(
			strconv.Itoa(t.Year()),
			fmt.Sprintf("%02d", int(t.Month())),
			fmt.Sprintf("%02d", t.Day()),
		),
		t.Format("150405") + ".jpg"
}

// Archive stores the clean master for later timelapses. It is a no-op when
// ArchiveDir is unset.
func (p *Publisher) Archive(t time.Time, data []byte) error {
	if p.ArchiveDir == "" {
		return nil
	}
	sub, name := ArchivePath(t)
	return p.WriteAtomic(filepath.Join(p.ArchiveDir, sub), name, data)
}

// Prune deletes archived days older than RetentionDays. It is a no-op when
// archiving is off or retention is unlimited.
//
// Whole day directories are removed rather than individual files, so the cost
// is proportional to days expiring rather than to frames retained.
func (p *Publisher) Prune(now time.Time) error {
	if p.ArchiveDir == "" || p.RetentionDays <= 0 {
		return nil
	}
	cutoff := now.AddDate(0, 0, -p.RetentionDays).Truncate(24 * time.Hour)

	years, err := readDirNames(p.ArchiveDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("publish: prune: %w", err)
	}

	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	for _, y := range years {
		year, err := strconv.Atoi(y)
		if err != nil {
			continue // not ours; leave it alone
		}
		yearDir := filepath.Join(p.ArchiveDir, y)

		months, err := readDirNames(yearDir)
		if err != nil {
			note(err)
			continue
		}
		for _, m := range months {
			month, err := strconv.Atoi(m)
			if err != nil || month < 1 || month > 12 {
				continue
			}
			monthDir := filepath.Join(yearDir, m)

			days, err := readDirNames(monthDir)
			if err != nil {
				note(err)
				continue
			}
			for _, d := range days {
				day, err := strconv.Atoi(d)
				if err != nil || day < 1 || day > 31 {
					continue
				}
				stamp := time.Date(year, time.Month(month), day, 0, 0, 0, 0, now.Location())
				if !stamp.Before(cutoff) {
					continue
				}
				if err := os.RemoveAll(filepath.Join(monthDir, d)); err != nil {
					note(fmt.Errorf("publish: prune %s: %w", filepath.Join(monthDir, d), err))
				}
			}
			removeIfEmpty(monthDir)
		}
		removeIfEmpty(yearDir)
	}
	return firstErr
}

// readDirNames lists a directory's entry names, sorted.
func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// removeIfEmpty drops a directory that pruning has emptied. A non-empty
// directory makes Remove fail, which is exactly the wanted behaviour.
func removeIfEmpty(dir string) {
	_ = os.Remove(dir)
}
