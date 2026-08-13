package publish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeStore records what was uploaded and can be made to fail.
type fakeStore struct {
	mu   sync.Mutex
	put  map[string]PutOptions
	body map[string][]byte
	fail error
}

func newFakeStore() *fakeStore {
	return &fakeStore{put: map[string]PutOptions{}, body: map[string][]byte{}}
}

func (f *fakeStore) Put(_ context.Context, name string, data []byte, opts PutOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.put[name] = opts
	f.body[name] = append([]byte(nil), data...)
	return nil
}

func (f *fakeStore) Close() error { return nil }

func (f *fakeStore) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.put))
	for k := range f.put {
		out = append(out, k)
	}
	return out
}

func TestPublishWritesLocallyAndUploads(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	p := &Publisher{OutputDir: dir, Quality: 92, Store: store}

	if err := p.Publish(context.Background(), "latest.jpg", []byte("frame")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if got, err := os.ReadFile(filepath.Join(dir, "latest.jpg")); err != nil || string(got) != "frame" {
		t.Errorf("local copy = %q (err=%v), want %q", got, err, "frame")
	}
	if string(store.body["latest.jpg"]) != "frame" {
		t.Errorf("uploaded %q, want %q", store.body["latest.jpg"], "frame")
	}
}

// A bucket that changes every minute must not be cached as a static asset.
// GCS defaults to public, max-age=3600, which would serve an hour-old frame.
func TestPublishSetsCacheControl(t *testing.T) {
	store := newFakeStore()
	p := &Publisher{OutputDir: t.TempDir(), Quality: 92, Store: store}

	if err := p.Publish(context.Background(), "latest.jpg", []byte("x")); err != nil {
		t.Fatal(err)
	}
	opts := store.put["latest.jpg"]
	if opts.CacheControl != defaultCacheControl {
		t.Errorf("CacheControl = %q, want %q", opts.CacheControl, defaultCacheControl)
	}
	if opts.ContentType != "image/jpeg" {
		t.Errorf("ContentType = %q, want image/jpeg", opts.ContentType)
	}
}

func TestPublishHonoursConfiguredCacheControl(t *testing.T) {
	store := newFakeStore()
	p := &Publisher{OutputDir: t.TempDir(), Quality: 92, Store: store, CacheControl: "max-age=30"}

	if err := p.Publish(context.Background(), "latest.jpg", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got := store.put["latest.jpg"].CacheControl; got != "max-age=30" {
		t.Errorf("CacheControl = %q, want max-age=30", got)
	}
}

// The frame reached disk; only the upload failed. The caller has to be able to
// tell those apart, because one is a stale feed and the other is a lost frame.
func TestPublishStoreFailureIsDistinguishableAndLocalStillLands(t *testing.T) {
	dir := t.TempDir()
	store := newFakeStore()
	store.fail = errors.New("bucket unreachable")
	p := &Publisher{OutputDir: dir, Quality: 92, Store: store}

	err := p.Publish(context.Background(), "latest.jpg", []byte("frame"))
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, ErrStore) {
		t.Errorf("error %v does not wrap ErrStore", err)
	}

	// The local copy is the fallback and must still be complete.
	if got, err := os.ReadFile(filepath.Join(dir, "latest.jpg")); err != nil || string(got) != "frame" {
		t.Errorf("local copy = %q (err=%v), want the frame to have landed anyway", got, err)
	}
}

// A local failure is not a store failure, or the daemon would carry on as if
// the frame had been written.
func TestPublishLocalFailureIsNotAStoreError(t *testing.T) {
	p := &Publisher{OutputDir: "", Quality: 92, Store: newFakeStore()}

	err := p.Publish(context.Background(), "latest.jpg", []byte("x"))
	if err == nil {
		t.Fatal("want an error")
	}
	if errors.Is(err, ErrStore) {
		t.Errorf("a local failure was reported as a store failure: %v", err)
	}
}

func TestPublishWithoutStore(t *testing.T) {
	dir := t.TempDir()
	p := &Publisher{OutputDir: dir, Quality: 92}

	if err := p.Publish(context.Background(), "latest.jpg", []byte("x")); err != nil {
		t.Fatalf("Publish with no store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "latest.jpg")); err != nil {
		t.Error(err)
	}
}

func TestObjectPrefix(t *testing.T) {
	store := newFakeStore()
	p := &Publisher{OutputDir: t.TempDir(), Quality: 92, Store: store, ObjectPrefix: "/valley-view/"}

	if err := p.Publish(context.Background(), "latest.jpg", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.put["valley-view/latest.jpg"]; !ok {
		t.Errorf("uploaded under %v, want valley-view/latest.jpg", store.names())
	}
}

// Archived frames are written once under a timestamped name and never change,
// so they may cache indefinitely — the opposite of latest.jpg.
func TestArchiveUploadsImmutableObject(t *testing.T) {
	store := newFakeStore()
	p := &Publisher{Quality: 92, Store: store, ArchiveToStore: true}

	ts := time.Date(2026, 3, 9, 7, 5, 4, 0, time.UTC)
	if err := p.Archive(context.Background(), ts, []byte("master")); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	const key = "archive/2026/03/09/070504.jpg"
	opts, ok := store.put[key]
	if !ok {
		t.Fatalf("uploaded %v, want %s", store.names(), key)
	}
	if opts.CacheControl != "public, max-age=31536000, immutable" {
		t.Errorf("CacheControl = %q, want it cacheable forever", opts.CacheControl)
	}
}

func TestArchiveToStoreCanBeDisabled(t *testing.T) {
	store := newFakeStore()
	p := &Publisher{Quality: 92, Store: store, ArchiveToStore: false}

	if err := p.Archive(context.Background(), time.Now(), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if n := len(store.names()); n != 0 {
		t.Errorf("uploaded %d objects with archiving off: %v", n, store.names())
	}
}

// Local and remote archiving are independent; one being off must not disable
// the other.
func TestArchiveLocalAndStoreAreIndependent(t *testing.T) {
	root := t.TempDir()
	store := newFakeStore()
	p := &Publisher{ArchiveDir: root, Quality: 92, Store: store, ArchiveToStore: true}

	ts := time.Date(2026, 3, 9, 7, 5, 4, 0, time.UTC)
	if err := p.Archive(context.Background(), ts, []byte("master")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "2026", "03", "09", "070504.jpg")); err != nil {
		t.Errorf("local archive missing: %v", err)
	}
	if _, ok := store.put["archive/2026/03/09/070504.jpg"]; !ok {
		t.Errorf("remote archive missing: %v", store.names())
	}
}

// fakeQueue records what was handed to the durable queue.
type fakeQueue struct {
	latest  []string
	archive []string
	fail    error
}

func (q *fakeQueue) AddLatest(object string, _ []byte) error {
	if q.fail != nil {
		return q.fail
	}
	q.latest = append(q.latest, object)
	return nil
}

func (q *fakeQueue) AddArchive(object string, _ []byte) error {
	if q.fail != nil {
		return q.fail
	}
	q.archive = append(q.archive, object)
	return nil
}

// With a queue configured the upload must not happen inline: rendering cannot
// be allowed to wait on a mountain-top network link.
func TestPublishPrefersTheQueueOverInlineUpload(t *testing.T) {
	store := newFakeStore()
	queue := &fakeQueue{}
	p := &Publisher{OutputDir: t.TempDir(), Quality: 92, Store: store, Queue: queue}

	if err := p.Publish(context.Background(), "latest.jpg", []byte("frame")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(queue.latest) != 1 || queue.latest[0] != "latest.jpg" {
		t.Errorf("queued %v, want latest.jpg", queue.latest)
	}
	if n := len(store.names()); n != 0 {
		t.Errorf("uploaded inline despite a queue: %v", store.names())
	}
}

func TestArchivePrefersTheQueue(t *testing.T) {
	store := newFakeStore()
	queue := &fakeQueue{}
	p := &Publisher{Quality: 92, Store: store, Queue: queue, ArchiveToStore: true}

	ts := time.Date(2026, 3, 9, 7, 5, 4, 0, time.UTC)
	if err := p.Archive(context.Background(), ts, []byte("master")); err != nil {
		t.Fatal(err)
	}
	if want := "archive/2026/03/09/070504.jpg"; len(queue.archive) != 1 || queue.archive[0] != want {
		t.Errorf("queued %v, want %s", queue.archive, want)
	}
	if n := len(store.names()); n != 0 {
		t.Errorf("uploaded inline despite a queue: %v", store.names())
	}
}

// A queue that cannot accept the frame is still a store-side problem: the frame
// is on local disk, so the daemon carries on rather than losing the cycle.
func TestQueueFailureIsAStoreError(t *testing.T) {
	queue := &fakeQueue{fail: errors.New("disk full")}
	p := &Publisher{OutputDir: t.TempDir(), Quality: 92, Queue: queue}

	err := p.Publish(context.Background(), "latest.jpg", []byte("x"))
	if err == nil {
		t.Fatal("want an error")
	}
	if !errors.Is(err, ErrStore) {
		t.Errorf("error %v does not wrap ErrStore", err)
	}
}
