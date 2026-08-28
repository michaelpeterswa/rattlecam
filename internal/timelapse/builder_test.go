package timelapse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- doubles -----------------------------------------------------------------

type putRecord struct {
	name         string
	contentType  string
	cacheControl string
	bytes        int
}

type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    []putRecord

	getErr  map[string]error
	listErr error
}

func newStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}, getErr: map[string]error{}}
}

func (s *fakeStore) List(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []string
	for name := range s.objects {
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *fakeStore) Get(_ context.Context, name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.getErr[name]; err != nil {
		return nil, err
	}
	data, ok := s.objects[name]
	if !ok {
		return nil, fmt.Errorf("no such object: %s", name)
	}
	return append([]byte(nil), data...), nil
}

func (s *fakeStore) Put(_ context.Context, name string, data []byte, contentType, cacheControl string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[name] = append([]byte(nil), data...)
	s.puts = append(s.puts, putRecord{name, contentType, cacheControl, len(data)})
	return nil
}

func (s *fakeStore) put(name string) (putRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.puts {
		if p.name == name {
			return p, true
		}
	}
	return putRecord{}, false
}

type encodeCall struct {
	frames []string
	dst    string
}

type fakeEncoder struct {
	mu      sync.Mutex
	encodes []encodeCall
	joins   []encodeCall
	err     error
}

func (e *fakeEncoder) Encode(_ context.Context, frames []string, dst string) error {
	e.mu.Lock()
	e.encodes = append(e.encodes, encodeCall{append([]string(nil), frames...), dst})
	err := e.err
	e.mu.Unlock()
	if err != nil {
		return err
	}
	return writeFile(dst, []byte("segment:"+strings.Join(baseNames(frames), ",")))
}

func (e *fakeEncoder) Join(_ context.Context, segments []string, dst string) error {
	e.mu.Lock()
	e.joins = append(e.joins, encodeCall{append([]string(nil), segments...), dst})
	e.mu.Unlock()
	return writeFile(dst, []byte("joined:"+strings.Join(baseNames(segments), ",")))
}

func baseNames(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}

// --- fixtures ----------------------------------------------------------------

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func testBuilder(t *testing.T, s *fakeStore, e *fakeEncoder) *Builder {
	t.Helper()
	return &Builder{
		Store:   s,
		Layout:  Layout{Fingerprint: "test"},
		Enc:     e,
		WorkDir: t.TempDir(),
		Workers: 4,
		Log:     quietLog(),
	}
}

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// archiveDay puts a day of frames in the store: day frames in colour, night
// frames in grey, in the same shape the daemon writes them.
func archiveDay(t *testing.T, s *fakeStore, l Layout, day time.Time, dayFrames, nightFrames int) {
	t.Helper()
	dir := t.TempDir()
	prefix := l.ArchiveDay(day)

	hour := 0
	add := func(mono bool) {
		p := filepath.Join(dir, fmt.Sprintf("%02d0000.jpg", hour))
		if mono {
			monoFrame(t, p)
		} else {
			colourFrame(t, p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		// Night first, so the day is not trivially sorted into day-then-night.
		s.objects[prefix+fmt.Sprintf("%02d0000.jpg", hour)] = data
		hour++
	}

	for range nightFrames {
		add(true)
	}
	for range dayFrames {
		add(false)
	}
}

// seedSegments puts already-built segments in the store, as a previous run
// would have left them.
func seedSegments(s *fakeStore, l Layout, days ...time.Time) {
	for _, d := range days {
		for _, v := range []Variant{Full, DaylightOnly} {
			s.objects[l.Segment(d, v)] = []byte("prebuilt " + d.Format(DateFormat) + " " + string(v))
		}
	}
}

// --- Ensure ------------------------------------------------------------------

// The whole point of keeping segments: an ordinary night encodes one day, not
// thirty.
func TestEnsureOnlyBuildsTheDaysTheBucketIsMissing(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)

	days := Window(date(2026, 8, 26), 3)
	seedSegments(s, b.Layout, days[0], days[1])
	archiveDay(t, s, b.Layout, days[2], 4, 2)

	ready, err := b.Ensure(context.Background(), days, 31)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if len(ready) != 3 {
		t.Errorf("ready = %d days, want 3", len(ready))
	}
	// Two variants for the one missing day, and nothing for the two present.
	if len(e.encodes) != 2 {
		t.Errorf("encoded %d segments, want 2 (the missing day's two variants)", len(e.encodes))
	}
	for _, c := range e.encodes {
		if !strings.Contains(c.dst, days[2].Format(DateFormat)) {
			t.Errorf("encoded %s, want only %s", filepath.Base(c.dst), days[2].Format(DateFormat))
		}
	}
}

// A first run against a long archive must not be able to exceed the job's
// timeout; the days it does not reach are picked up by the next run.
func TestEnsureStopsBuildingAtTheBudget(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)

	days := Window(date(2026, 8, 26), 5)
	for _, d := range days {
		archiveDay(t, s, b.Layout, d, 3, 1)
	}

	ready, err := b.Ensure(context.Background(), days, 2)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if len(ready) != 2 {
		t.Errorf("ready = %d days, want 2 (the budget)", len(ready))
	}
	if len(e.encodes) != 4 {
		t.Errorf("encoded %d segments, want 4 (two days, two variants)", len(e.encodes))
	}
	// The budget must spend itself on the oldest days first, so a repeated run
	// walks forward rather than rebuilding the same end of the window.
	for _, c := range e.encodes {
		if strings.Contains(c.dst, days[4].Format(DateFormat)) {
			t.Errorf("spent budget on %s, the newest day, leaving older ones unbuilt", days[4].Format(DateFormat))
		}
	}
}

// The camera or the link was down. That is not a failure of the run.
func TestEnsureLeavesOutADayWithNothingArchived(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)

	days := Window(date(2026, 8, 26), 3)
	archiveDay(t, s, b.Layout, days[0], 3, 1)
	// days[1] is missing entirely.
	archiveDay(t, s, b.Layout, days[2], 3, 1)

	ready, err := b.Ensure(context.Background(), days, 31)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if len(ready) != 2 {
		t.Fatalf("ready = %d days, want 2", len(ready))
	}
	for _, d := range ready {
		if d.Equal(days[1]) {
			t.Errorf("%s has no frames but was reported ready", d.Format(DateFormat))
		}
	}
}

func TestEnsureReportsAFailedListing(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	s.listErr = errors.New("bucket unreachable")
	b := testBuilder(t, s, e)

	if _, err := b.Ensure(context.Background(), Window(date(2026, 8, 26), 3), 31); err == nil {
		t.Fatal("Ensure succeeded with an unreachable bucket")
	}
}

// A segment that will not download is one day of a month. Losing the other
// twenty-nine over it would be the wrong trade.
func TestProductLeavesOutASegmentThatWillNotDownload(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)

	days := Window(date(2026, 8, 26), 3)
	seedSegments(s, b.Layout, days...)
	s.getErr[b.Layout.Segment(days[0], Full)] = errors.New("read timeout")

	if _, err := b.Ensure(context.Background(), days, 31); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := b.Product(context.Background(), Weekly, days[2], days, Full); err != nil {
		t.Fatalf("Product: %v", err)
	}

	if got := len(e.joins[0].segments()); got != 2 {
		t.Errorf("joined %d segments, want the 2 that downloaded", got)
	}
}

// Downloading both variants for every day would pull the full segment for all
// thirty when only the last seven use it.
func TestOnlyTheVariantAProductUsesIsDownloaded(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)

	days := Window(date(2026, 8, 26), 3)
	seedSegments(s, b.Layout, days...)

	if _, err := b.Ensure(context.Background(), days, 31); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// Ensure alone must not have fetched anything: the bucket listing already
	// told it the days were usable.
	for _, d := range days {
		for _, v := range []Variant{Full, DaylightOnly} {
			if _, err := os.Stat(b.segmentPath(d, v)); err == nil {
				t.Errorf("Ensure downloaded the %s segment for %s before knowing it was wanted",
					v, d.Format(DateFormat))
			}
		}
	}

	if err := b.Product(context.Background(), Monthly, days[2], days, DaylightOnly); err != nil {
		t.Fatalf("Product: %v", err)
	}
	for _, d := range days {
		if _, err := os.Stat(b.segmentPath(d, Full)); err == nil {
			t.Errorf("the full segment for %s was downloaded for a daylight-only product", d.Format(DateFormat))
		}
	}
}

// --- segments ----------------------------------------------------------------

func TestBuildingADayUploadsBothVariants(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)
	day := date(2026, 8, 26)
	archiveDay(t, s, b.Layout, day, 5, 3)

	if _, err := b.Ensure(context.Background(), []time.Time{day}, 31); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	for _, v := range []Variant{Full, DaylightOnly} {
		rec, ok := s.put(b.Layout.Segment(day, v))
		if !ok {
			t.Errorf("the %s segment was never uploaded", v)
			continue
		}
		if rec.contentType != videoType {
			t.Errorf("%s content type = %q, want %q", v, rec.contentType, videoType)
		}
		if rec.cacheControl != immutableCache {
			t.Errorf("%s cache control = %q, want it immutable", v, rec.cacheControl)
		}
	}
}

func TestTheDaylightSegmentHoldsOnlyTheColourFrames(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)
	day := date(2026, 8, 26)
	archiveDay(t, s, b.Layout, day, 5, 3)

	if _, err := b.Ensure(context.Background(), []time.Time{day}, 31); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	var full, daylight int
	for _, c := range e.encodes {
		if strings.Contains(c.dst, string(DaylightOnly)) {
			daylight = len(c.frames)
		} else {
			full = len(c.frames)
		}
	}
	if full != 8 {
		t.Errorf("the full segment got %d frames, want 8", full)
	}
	if daylight != 5 {
		t.Errorf("the daylight segment got %d frames, want 5", daylight)
	}
}

// A whole day in fog would otherwise produce an empty daylight segment, which
// ffmpeg cannot encode — and the day would then be missing from the monthly for
// good, because a built segment is never revisited.
func TestADayWithNoDaylightGoesIntoTheMonthlyWhole(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)
	day := date(2026, 12, 21)
	archiveDay(t, s, b.Layout, day, 0, 6)

	ready, err := b.Ensure(context.Background(), []time.Time{day}, 31)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(ready) != 1 {
		t.Fatalf("ready = %v, want the day to be usable", ready)
	}

	for _, c := range e.encodes {
		if len(c.frames) != 6 {
			t.Errorf("%s got %d frames, want all 6", filepath.Base(c.dst), len(c.frames))
		}
	}
}

// A day is 125 MB of frames and a backfill walks thirty of them. On Cloud Run
// the work directory is memory, so the high-water mark has to stay at one day.
func TestFramesAreDeletedOnceTheDayIsEncoded(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)

	days := Window(date(2026, 8, 26), 3)
	for _, d := range days {
		archiveDay(t, s, b.Layout, d, 3, 1)
	}

	if _, err := b.Ensure(context.Background(), days, 31); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	for _, d := range days {
		if _, err := os.Stat(b.framesDir(d)); !os.IsNotExist(err) {
			t.Errorf("%s frames are still on disk after encoding: %v", d.Format(DateFormat), err)
		}
	}
}

func TestOneFrameThatWillNotDownloadDoesNotLoseTheDay(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)
	day := date(2026, 8, 26)
	archiveDay(t, s, b.Layout, day, 4, 0)

	names, _ := s.List(context.Background(), b.Layout.ArchiveDay(day))
	s.getErr[names[0]] = errors.New("read timeout")

	if _, err := b.Ensure(context.Background(), []time.Time{day}, 31); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	for _, c := range e.encodes {
		if len(c.frames) != 3 {
			t.Errorf("%s got %d frames, want the 3 that downloaded", filepath.Base(c.dst), len(c.frames))
		}
	}
}

// Anything that is not a frame under the archive prefix must not reach ffmpeg.
func TestNonFramesUnderTheArchivePrefixAreIgnored(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)
	day := date(2026, 8, 26)
	archiveDay(t, s, b.Layout, day, 3, 0)
	s.objects[b.Layout.ArchiveDay(day)+"NOTES.txt"] = []byte("left by hand")

	if _, err := b.Ensure(context.Background(), []time.Time{day}, 31); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	for _, c := range e.encodes {
		for _, f := range c.frames {
			if !strings.HasSuffix(f, ".jpg") {
				t.Errorf("%s reached the encoder", filepath.Base(f))
			}
		}
	}
}

// --- products ----------------------------------------------------------------

func TestProductUploadsTheDatedCopyAndTheStableName(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)

	days := Window(date(2026, 8, 26), 3)
	for _, d := range days {
		archiveDay(t, s, b.Layout, d, 3, 1)
	}
	if _, err := b.Ensure(context.Background(), days, 31); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if err := b.Product(context.Background(), Weekly, days[2], days, Full); err != nil {
		t.Fatalf("Product: %v", err)
	}

	dated, ok := s.put(b.Layout.Product(Weekly, days[2]))
	if !ok {
		t.Fatal("the dated copy was never uploaded")
	}
	if dated.cacheControl != immutableCache {
		t.Errorf("dated cache control = %q, want it immutable — it never changes", dated.cacheControl)
	}

	latest, ok := s.put(b.Layout.Latest(Weekly))
	if !ok {
		t.Fatal("latest-weekly was never uploaded")
	}
	if latest.cacheControl != latestCache {
		t.Errorf("latest cache control = %q, want the short one — it is rewritten nightly", latest.cacheControl)
	}
	if latest.bytes != dated.bytes {
		t.Errorf("latest is %d bytes and the dated copy %d; they should be the same video", latest.bytes, dated.bytes)
	}
}

// A month assembled out of order is a month that jumps back and forth in time.
func TestProductJoinsSegmentsOldestFirst(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)

	days := Window(date(2026, 8, 26), 4)
	for _, d := range days {
		archiveDay(t, s, b.Layout, d, 3, 1)
	}
	if _, err := b.Ensure(context.Background(), days, 31); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if err := b.Product(context.Background(), Monthly, days[3], days, DaylightOnly); err != nil {
		t.Fatalf("Product: %v", err)
	}

	if len(e.joins) != 1 {
		t.Fatalf("joined %d times, want 1", len(e.joins))
	}
	got := baseNames(e.joins[0].segments())
	want := []string{
		"2026-08-23-daylight.mp4",
		"2026-08-24-daylight.mp4",
		"2026-08-25-daylight.mp4",
		"2026-08-26-daylight.mp4",
	}
	if len(got) != len(want) {
		t.Fatalf("joined %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("segment %d = %s, want %s", i, got[i], want[i])
		}
	}
}

// The month is the one that drops the night; the week keeps it.
func TestTheMonthlyTakesDaylightSegmentsAndTheWeeklyTakesFullOnes(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)

	days := Window(date(2026, 8, 26), 2)
	for _, d := range days {
		archiveDay(t, s, b.Layout, d, 3, 1)
	}
	if _, err := b.Ensure(context.Background(), days, 31); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if err := b.Product(context.Background(), Weekly, days[1], days, Full); err != nil {
		t.Fatalf("weekly: %v", err)
	}
	if err := b.Product(context.Background(), Monthly, days[1], days, DaylightOnly); err != nil {
		t.Fatalf("monthly: %v", err)
	}

	for _, f := range baseNames(e.joins[0].segments()) {
		if strings.Contains(f, "daylight") {
			t.Errorf("the weekly joined %s, which drops the night", f)
		}
	}
	for _, f := range baseNames(e.joins[1].segments()) {
		if !strings.Contains(f, "daylight") {
			t.Errorf("the monthly joined %s, which keeps the night", f)
		}
	}
}

// A day whose segment could not be built is skipped rather than joined as a
// missing file, which ffmpeg would fail on.
func TestProductSkipsADayWithNoSegment(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)

	days := Window(date(2026, 8, 26), 3)
	archiveDay(t, s, b.Layout, days[0], 3, 1)
	archiveDay(t, s, b.Layout, days[2], 3, 1)
	if _, err := b.Ensure(context.Background(), days, 31); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if err := b.Product(context.Background(), Weekly, days[2], days, Full); err != nil {
		t.Fatalf("Product: %v", err)
	}
	if got := len(e.joins[0].segments()); got != 2 {
		t.Errorf("joined %d segments, want the 2 that exist", got)
	}
}

// seedSegments leaves nothing on local disk, so a product built straight from
// them exercises the fetch path rather than files a build happened to leave.
func TestProductFetchesSegmentsItDidNotBuild(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)

	days := Window(date(2026, 8, 26), 7)
	seedSegments(s, b.Layout, days...)

	ready, err := b.Ensure(context.Background(), days, 0)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(ready) != 7 {
		t.Fatalf("ready = %d, want 7 without encoding anything", len(ready))
	}
	if len(e.encodes) != 0 {
		t.Errorf("encoded %d segments, want none — they were all already built", len(e.encodes))
	}

	if err := b.Product(context.Background(), Weekly, days[6], days, Full); err != nil {
		t.Fatalf("Product: %v", err)
	}
	if got := len(e.joins[0].segments()); got != 7 {
		t.Errorf("joined %d segments, want 7", got)
	}
}

func TestProductFailsWhenNothingIsAvailableToJoin(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)

	err := b.Product(context.Background(), Monthly, date(2026, 8, 26), Window(date(2026, 8, 26), 30), DaylightOnly)
	if err == nil {
		t.Fatal("building a product from nothing succeeded")
	}
	if !strings.Contains(err.Error(), "no segments") {
		t.Errorf("error = %v, want it to say there were no segments", err)
	}
}

func TestDailyProductIsJustTheOneDay(t *testing.T) {
	s, e := newStore(), &fakeEncoder{}
	b := testBuilder(t, s, e)
	day := date(2026, 8, 26)
	archiveDay(t, s, b.Layout, day, 4, 2)

	if _, err := b.Ensure(context.Background(), []time.Time{day}, 31); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := b.Product(context.Background(), Daily, day, Window(day, 1), Full); err != nil {
		t.Fatalf("Product: %v", err)
	}

	if got := len(e.joins[0].segments()); got != 1 {
		t.Errorf("the daily joined %d segments, want 1", got)
	}
	if _, ok := s.put(b.Layout.Latest(Daily)); !ok {
		t.Error("latest-daily was never uploaded")
	}
}

// segments is a small accessor so the join assertions read as what they check.
func (c encodeCall) segments() []string { return c.frames }
