// Package publish encodes frames and gets them onto disk without a reader ever
// seeing a half-written file.
package publish

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
)

// Publisher writes the public frame, the clean frame and the archive.
type Publisher struct {
	OutputDir     string
	ArchiveDir    string // empty disables local archiving
	Quality       int
	RetentionDays int // 0 keeps local archives forever

	// Store, when set, receives every frame in addition to the local copy.
	// Local stays the source of truth so that losing the bucket degrades the
	// feed to "stale" rather than stopping it.
	Store          ObjectStore
	ObjectPrefix   string // optional key prefix inside the bucket
	CacheControl   string // applied to the latest-* objects
	ArchiveToStore bool

	// Queue, when set, receives frames instead of Store doing so inline.
	//
	// That keeps the upload off the render path: a slow or dead link delays
	// nothing, because handing a frame to the queue is a local disk write. A
	// separate drainer moves it on when the network allows.
	Queue Enqueuer
}

// Enqueuer is the durable hand-off between rendering and uploading.
type Enqueuer interface {
	// AddLatest queues a frame that supersedes any pending one of the same
	// name.
	AddLatest(object string, data []byte) error
	// AddArchive queues a frame that must not be superseded.
	AddArchive(object string, data []byte) error
}

// defaultCacheControl keeps a stable URL from serving a stale frame. A bucket's
// own default is public, max-age=3600, which for a file that changes every
// minute is exactly wrong.
const defaultCacheControl = "no-cache, max-age=0, must-revalidate"

func (p *Publisher) cacheControl() string {
	if p.CacheControl == "" {
		return defaultCacheControl
	}
	return p.CacheControl
}

// object applies the configured prefix to a key. Object names always use
// forward slashes, whatever the local filesystem does.
func (p *Publisher) object(name string) string {
	name = filepath.ToSlash(name)
	if p.ObjectPrefix == "" {
		return name
	}
	return path.Join(strings.Trim(p.ObjectPrefix, "/"), name)
}

// Publish writes a frame to the output directory and, when a store is
// configured, uploads it as well.
//
// The local write happens first and its failure is returned plainly. A store
// failure comes back wrapping ErrStore, so a caller can log it and carry on:
// the frame is on disk, and the feed being a minute stale beats the daemon
// treating the whole cycle as lost.
func (p *Publisher) Publish(ctx context.Context, name string, data []byte) error {
	if err := p.WriteAtomic(p.OutputDir, name, data); err != nil {
		return err
	}
	if p.Queue != nil {
		return storeErr("queue "+name, p.Queue.AddLatest(p.object(name), data))
	}
	if p.Store == nil {
		return nil
	}
	return storeErr("put "+name, p.Store.Put(ctx, p.object(name), data, PutOptions{
		ContentType:  "image/jpeg",
		CacheControl: p.cacheControl(),
	}))
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

// EncodeScaled encodes img narrowed to width, keeping its aspect ratio.
//
// The published frame is 4K because outlets composite their own graphics onto
// it and want the resolution. A web page does not: at 1280 wide the same frame
// is a fraction of the bytes and indistinguishable in a browser, which is the
// difference between a page costing megabytes per view and costing hundreds of
// kilobytes.
//
// It never enlarges. Asking for more pixels than the camera produced would cost
// bytes to invent detail that is not there.
func (p *Publisher) EncodeScaled(img image.Image, width int) ([]byte, error) {
	b := img.Bounds()
	if width <= 0 || width >= b.Dx() || b.Dx() == 0 {
		return p.Encode(img)
	}

	height := int(float64(b.Dy()) * float64(width) / float64(b.Dx()))
	if height < 1 {
		height = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Src, nil)
	return p.Encode(dst)
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

// Archive stores the clean master for later timelapses, locally when
// ArchiveDir is set and in the object store when ArchiveToStore is on. Either
// may be disabled independently.
func (p *Publisher) Archive(ctx context.Context, t time.Time, data []byte) error {
	sub, name := ArchivePath(t)

	if p.ArchiveDir != "" {
		if err := p.WriteAtomic(filepath.Join(p.ArchiveDir, sub), name, data); err != nil {
			return err
		}
	}

	if !p.ArchiveToStore || (p.Store == nil && p.Queue == nil) {
		return nil
	}

	key := p.object(path.Join("archive", filepath.ToSlash(sub), name))

	if p.Queue != nil {
		return storeErr("queue "+key, p.Queue.AddArchive(key, data))
	}
	// An archived frame is written once under a timestamped name and never
	// changes, so unlike latest.jpg it can be cached indefinitely.
	return storeErr("put "+key, p.Store.Put(ctx, key, data, PutOptions{
		ContentType:  "image/jpeg",
		CacheControl: "public, max-age=31536000, immutable",
	}))
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
