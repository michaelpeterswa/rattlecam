package publish

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWriteAtomicWritesCompleteFile(t *testing.T) {
	dir := t.TempDir()
	p := &Publisher{OutputDir: dir, Quality: 92}

	want := bytes.Repeat([]byte("frame"), 1000)
	if err := p.WriteAtomic(dir, "latest.jpg", want); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "latest.jpg"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("content differs: got %d bytes, want %d", len(got), len(want))
	}
}

// A web server must never be able to read a torn frame. A reader hammering the
// path while writes land has to see one whole version or the other, never a
// prefix and never a missing file.
func TestWriteAtomicNeverExposesAPartialFile(t *testing.T) {
	dir := t.TempDir()
	p := &Publisher{OutputDir: dir, Quality: 92}
	path := filepath.Join(dir, "latest.jpg")

	// Big enough that a non-atomic write would be split across syscalls.
	versions := [][]byte{
		bytes.Repeat([]byte{'A'}, 3<<20),
		bytes.Repeat([]byte{'B'}, 3<<20),
	}
	if err := p.WriteAtomic(dir, "latest.jpg", versions[0]); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	var (
		mu    sync.Mutex
		torn  []string
		reads int
	)

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				got, err := os.ReadFile(path)
				if err != nil {
					mu.Lock()
					torn = append(torn, "read failed: "+err.Error())
					mu.Unlock()
					return
				}
				ok := bytes.Equal(got, versions[0]) || bytes.Equal(got, versions[1])
				mu.Lock()
				reads++
				if !ok {
					torn = append(torn, "partial read of "+string(rune('0'+len(got)%10))+
						" bytes: len="+itoa(len(got)))
				}
				mu.Unlock()
			}
		}()
	}

	for i := range 40 {
		if err := p.WriteAtomic(dir, "latest.jpg", versions[i%2]); err != nil {
			cancel()
			wg.Wait()
			t.Fatalf("WriteAtomic: %v", err)
		}
	}
	cancel()
	wg.Wait()

	if reads == 0 {
		t.Fatal("readers never observed the file")
	}
	if len(torn) > 0 {
		t.Errorf("observed %d torn reads out of %d, e.g. %s", len(torn), reads, torn[0])
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A failed write must not leave temp files lying around in a directory a web
// server is serving.
func TestWriteAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	p := &Publisher{OutputDir: dir, Quality: 92}

	for range 5 {
		if err := p.WriteAtomic(dir, "latest.jpg", []byte("x")); err != nil {
			t.Fatalf("WriteAtomic: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "latest.jpg" {
			t.Errorf("stray file left behind: %s", e.Name())
		}
	}
}

func TestWriteAtomicIsWorldReadable(t *testing.T) {
	dir := t.TempDir()
	p := &Publisher{OutputDir: dir, Quality: 92}
	if err := p.WriteAtomic(dir, "latest.jpg", []byte("x")); err != nil {
		t.Fatal(err)
	}

	// CreateTemp makes 0600 files; a web server running as another user needs
	// to be able to read the published frame.
	info, err := os.Stat(filepath.Join(dir, "latest.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o044 == 0 {
		t.Errorf("mode = %v, want group/other readable", perm)
	}
}

func TestWriteAtomicCreatesMissingDirectories(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b", "c")
	p := &Publisher{Quality: 92}
	if err := p.WriteAtomic(dir, "latest.jpg", []byte("x")); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "latest.jpg")); err != nil {
		t.Fatal(err)
	}
}

func TestEncodeProducesDecodableJPEG(t *testing.T) {
	p := &Publisher{Quality: 92}

	src := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := range 48 {
		for x := range 64 {
			src.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 5), B: 120, A: 255})
		}
	}

	data, err := p.Encode(src)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := img.Bounds(); got != src.Bounds() {
		t.Errorf("bounds = %v, want %v", got, src.Bounds())
	}
}

func TestEncodeQualityFallback(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for _, q := range []int{0, -1, 101} {
		p := &Publisher{Quality: q}
		if _, err := p.Encode(src); err != nil {
			t.Errorf("Encode with quality %d: %v", q, err)
		}
	}
}

func TestArchivePath(t *testing.T) {
	ts := time.Date(2026, 3, 9, 7, 5, 4, 0, time.UTC)
	dir, name := ArchivePath(ts)
	if want := filepath.Join("2026", "03", "09"); dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
	if want := "070504.jpg"; name != want {
		t.Errorf("name = %q, want %q", name, want)
	}
}

func TestArchiveIsNoOpWithoutArchiveDir(t *testing.T) {
	p := &Publisher{OutputDir: t.TempDir(), Quality: 92}
	if err := p.Archive(context.Background(), time.Now(), []byte("x")); err != nil {
		t.Errorf("Archive with no ArchiveDir: %v", err)
	}
}

func TestArchiveWritesDatedPath(t *testing.T) {
	root := t.TempDir()
	p := &Publisher{ArchiveDir: root, Quality: 92}

	ts := time.Date(2026, 3, 9, 7, 5, 4, 0, time.UTC)
	if err := p.Archive(context.Background(), ts, []byte("master")); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "2026", "03", "09", "070504.jpg"))
	if err != nil {
		t.Fatalf("archived file: %v", err)
	}
	if string(got) != "master" {
		t.Errorf("content = %q, want %q", got, "master")
	}
}

func TestPrune(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	p := &Publisher{ArchiveDir: root, Quality: 92, RetentionDays: 7}

	keep := now.AddDate(0, 0, -2)
	drop := now.AddDate(0, 0, -30)
	for _, ts := range []time.Time{keep, drop} {
		if err := p.Archive(context.Background(), ts, []byte("x")); err != nil {
			t.Fatalf("seed archive: %v", err)
		}
	}

	if err := p.Prune(now); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	keepDir, _ := ArchivePath(keep)
	if _, err := os.Stat(filepath.Join(root, keepDir)); err != nil {
		t.Errorf("retained day was pruned: %v", err)
	}
	dropDir, _ := ArchivePath(drop)
	if _, err := os.Stat(filepath.Join(root, dropDir)); !os.IsNotExist(err) {
		t.Errorf("expired day survived pruning (err=%v)", err)
	}
}

func TestPruneUnlimitedRetentionKeepsEverything(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	p := &Publisher{ArchiveDir: root, Quality: 92, RetentionDays: 0}

	old := now.AddDate(-3, 0, 0)
	if err := p.Archive(context.Background(), old, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := p.Prune(now); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	dir, _ := ArchivePath(old)
	if _, err := os.Stat(filepath.Join(root, dir)); err != nil {
		t.Errorf("RetentionDays=0 should keep archives forever: %v", err)
	}
}

// Pruning must not wander outside the YYYY/MM/DD layout it owns.
func TestPruneIgnoresForeignDirectories(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	p := &Publisher{ArchiveDir: root, Quality: 92, RetentionDays: 1}

	foreign := filepath.Join(root, "notes")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := p.Prune(now); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(filepath.Join(foreign, "keep.txt")); err != nil {
		t.Errorf("prune touched a directory it does not own: %v", err)
	}
}

func TestPruneMissingArchiveDirIsNotAnError(t *testing.T) {
	p := &Publisher{ArchiveDir: filepath.Join(t.TempDir(), "nope"), RetentionDays: 7}
	if err := p.Prune(time.Now()); err != nil {
		t.Errorf("Prune on a missing archive dir: %v", err)
	}
}

func TestWriteAtomicRejectsEmptyDir(t *testing.T) {
	p := &Publisher{Quality: 92}
	err := p.WriteAtomic("", "latest.jpg", []byte("x"))
	if err == nil {
		t.Fatal("want an error for an empty directory")
	}
	if !strings.Contains(err.Error(), "latest.jpg") {
		t.Errorf("error %q does not name the file", err)
	}
}
