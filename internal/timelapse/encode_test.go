package timelapse

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// argValue returns the value following a flag, and whether it was present.
func argValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func TestFingerprintDistinguishesEverySettingThatBreaksAJoin(t *testing.T) {
	base := &Encoder{Width: 1280, FPS: 24, CRF: 25}
	seen := map[string]string{base.Fingerprint(): "base"}

	for name, e := range map[string]*Encoder{
		"width": {Width: 1920, FPS: 24, CRF: 25},
		"fps":   {Width: 1280, FPS: 30, CRF: 25},
		"crf":   {Width: 1280, FPS: 24, CRF: 21},
	} {
		fp := e.Fingerprint()
		if prev, ok := seen[fp]; ok {
			t.Errorf("changing the %s produced the same fingerprint %q as %s", name, fp, prev)
		}
		seen[fp] = name
	}
}

func TestFingerprintUsesDefaultsForUnsetFields(t *testing.T) {
	if got, want := (&Encoder{}).Fingerprint(), (&Encoder{
		Width: DefaultWidth, FPS: DefaultFPS, CRF: DefaultCRF,
	}).Fingerprint(); got != want {
		t.Errorf("zero encoder = %q, want %q", got, want)
	}
}

// Scene detection and the keyframe interval are what make segments joinable. If
// either drifts out of the command line, days stop concatenating cleanly and
// the failure is a corrupt month rather than an error.
func TestEncodeArgsPinTheSettingsAJoinDependsOn(t *testing.T) {
	e := &Encoder{FPS: 24, Width: 1280, CRF: 25}
	args := e.encodeArgs("list.txt", "out.mp4", 0)

	for flag, want := range map[string]string{
		"-g":            "48",
		"-keyint_min":   "48",
		"-sc_threshold": "0",
		"-crf":          "25",
		"-pix_fmt":      "yuv420p",
	} {
		got, ok := argValue(args, flag)
		if !ok {
			t.Errorf("%s is missing from the command line", flag)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
}

// The rate has to be set before the input as well as after it: the first tells
// the concat demuxer how long to hold each still, the second sets the output
// rate. With only the second, ffmpeg assumes 25 and drops or repeats frames.
func TestEncodeArgsSetTheFrameRateOnBothSidesOfTheInput(t *testing.T) {
	args := (&Encoder{FPS: 24}).encodeArgs("list.txt", "out.mp4", 0)

	input := -1
	var before, after bool
	for i, a := range args {
		switch {
		case a == "-i":
			input = i
		case a == "-r" && input < 0:
			before = true
		case a == "-r" && input >= 0:
			after = true
		}
	}
	if !before {
		t.Error("no -r before -i; the concat demuxer will assume a rate")
	}
	if !after {
		t.Error("no -r after -i")
	}
}

func TestEncodeArgsScaleToTheConfiguredWidth(t *testing.T) {
	filters, ok := argValue((&Encoder{Width: 1920}).encodeArgs("l", "o", 0), "-vf")
	if !ok {
		t.Fatal("no filter chain")
	}
	if !strings.Contains(filters, "scale=1920:-2") {
		t.Errorf("filters = %q, want a scale to 1920", filters)
	}
	// -2 rather than -1 so the height stays even, which yuv420p requires.
	if strings.Contains(filters, "scale=1920:-1") {
		t.Error("height is rounded to -1, which can produce an odd height yuv420p cannot encode")
	}
}

// The camera meters every frame independently ten minutes apart, so without
// this the result strobes.
func TestEncodeArgsDeflicker(t *testing.T) {
	filters, _ := argValue((&Encoder{}).encodeArgs("l", "o", 0), "-vf")
	if !strings.Contains(filters, "deflicker") {
		t.Errorf("filters = %q, want deflicker", filters)
	}
}

func TestJoinArgsCopyRatherThanReEncode(t *testing.T) {
	args := joinArgs("list.txt", "out.mp4")

	if got, _ := argValue(args, "-c"); got != "copy" {
		t.Errorf("-c = %q, want copy: joining must not re-encode", got)
	}
	for _, forbidden := range []string{"-crf", "-vf", "libx264"} {
		for _, a := range args {
			if a == forbidden {
				t.Errorf("%q appears in a join, which means it is re-encoding", forbidden)
			}
		}
	}
}

// A browser has to be able to start playing before the whole file arrives; on
// the monthly that is the difference between immediate and ninety megabytes.
func TestBothCommandLinesMoveTheIndexToTheFront(t *testing.T) {
	for name, args := range map[string][]string{
		"encode": (&Encoder{}).encodeArgs("l", "o", 0),
		"join":   joinArgs("l", "o"),
	} {
		if got, _ := argValue(args, "-movflags"); got != "+faststart" {
			t.Errorf("%s -movflags = %q, want +faststart", name, got)
		}
	}
}

func TestEncodeRejectsAnEmptyFrameList(t *testing.T) {
	err := (&Encoder{}).Encode(context.Background(), nil, filepath.Join(t.TempDir(), "out.mp4"))
	if err == nil {
		t.Fatal("encoding no frames succeeded")
	}
	if !strings.Contains(err.Error(), "no frames") {
		t.Errorf("error = %v, want it to say there were no frames", err)
	}
}

func TestJoinRejectsAnEmptySegmentList(t *testing.T) {
	err := (&Encoder{}).Join(context.Background(), nil, filepath.Join(t.TempDir(), "out.mp4"))
	if err == nil {
		t.Fatal("joining no segments succeeded")
	}
}

// The concat demuxer single-quotes its paths and has no escape for a quote
// inside one, so a directory with an apostrophe in it has to close the quote,
// emit an escaped one, and reopen.
func TestConcatListSurvivesAQuoteInThePath(t *testing.T) {
	dir := t.TempDir()
	frame := filepath.Join(dir, "o'brien.jpg")
	if err := os.WriteFile(frame, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	list, cleanup, err := writeConcatList([]string{frame}, dir, "frames")
	if err != nil {
		t.Fatalf("writeConcatList: %v", err)
	}
	defer cleanup()

	body, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `'\''`) {
		t.Errorf("list = %q, want the quote escaped as '\\''", body)
	}
}

func TestConcatListIsRemovedAfterUse(t *testing.T) {
	dir := t.TempDir()
	list, cleanup, err := writeConcatList([]string{filepath.Join(dir, "a.jpg")}, dir, "frames")
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Stat(list); !os.IsNotExist(err) {
		t.Errorf("the concat list is still there after cleanup: %v", err)
	}
}

func TestConcatListHoldsOneLinePerPathInOrder(t *testing.T) {
	dir := t.TempDir()
	paths := []string{
		filepath.Join(dir, "000425.jpg"),
		filepath.Join(dir, "001425.jpg"),
		filepath.Join(dir, "002425.jpg"),
	}

	list, cleanup, err := writeConcatList(paths, dir, "frames")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	body, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != len(paths) {
		t.Fatalf("got %d lines, want %d", len(lines), len(paths))
	}
	for i, p := range paths {
		if !strings.Contains(lines[i], filepath.Base(p)) {
			t.Errorf("line %d = %q, want it to name %s", i, lines[i], filepath.Base(p))
		}
	}
}

func TestRunReportsWhyFFmpegFailed(t *testing.T) {
	// A binary that certainly does not exist, so the error comes from the
	// attempt to run it rather than from ffmpeg's own output.
	e := &Encoder{FFmpeg: filepath.Join(t.TempDir(), "not-ffmpeg"), Timeout: time.Second}

	err := e.run(context.Background(), []string{"-version"})
	if err == nil {
		t.Fatal("running a missing binary succeeded")
	}
	if !strings.Contains(err.Error(), "ffmpeg") {
		t.Errorf("error = %v, want it to mention ffmpeg", err)
	}
}

func TestLastLinesKeepsTheTail(t *testing.T) {
	got := lastLines("banner\nmore banner\nthe actual error\n", 2)
	if !strings.Contains(got, "the actual error") {
		t.Errorf("lastLines = %q, want the last line", got)
	}
	if strings.Contains(got, "banner\nmore") {
		t.Errorf("lastLines = %q, want only the tail", got)
	}
}

// --- branding ---------------------------------------------------------------

// The logo is burned into the segment, so a branded segment and an unbranded one
// are not interchangeable even though every codec setting matches. Without this
// in the fingerprint, turning branding on would leave a month stitched from a
// mixture of the two, with the crest flickering in and out across day
// boundaries.
func TestFingerprintSeparatesBrandedFromUnbranded(t *testing.T) {
	plain := (&Encoder{Width: 1280, FPS: 24, CRF: 25}).Fingerprint()
	branded := (&Encoder{Width: 1280, FPS: 24, CRF: 25, Logo: "/tmp/logo.png"}).Fingerprint()

	if plain == branded {
		t.Fatalf("branded and unbranded share the fingerprint %q", plain)
	}
}

// Resizing the crest changes the pictures, so it has to invalidate them too.
func TestFingerprintTracksTheLogoSize(t *testing.T) {
	a := (&Encoder{Logo: "/tmp/logo.png", LogoHeight: 0.34}).Fingerprint()
	b := (&Encoder{Logo: "/tmp/logo.png", LogoHeight: 0.20}).Fingerprint()

	if a == b {
		t.Fatalf("two logo heights share the fingerprint %q", a)
	}
}

func TestUnbrandedEncodeUsesAPlainFilterChain(t *testing.T) {
	args := (&Encoder{}).encodeArgs("list.txt", "out.mp4", 0)

	if _, ok := argValue(args, "-vf"); !ok {
		t.Error("no -vf; an unbranded encode needs no filter_complex")
	}
	if _, ok := argValue(args, "-filter_complex"); ok {
		t.Error("-filter_complex on an unbranded encode")
	}
}

// A second input cannot be composited through -vf, so branding has to switch the
// chain to filter_complex and map its output explicitly.
func TestBrandedEncodeCompositesTheLogoTopLeft(t *testing.T) {
	e := &Encoder{Width: 1280, Logo: "/tmp/logo.png", LogoHeight: 0.34, LogoMargin: 0.025}
	args := e.encodeArgs("list.txt", "out.mp4", 720)

	chain, ok := argValue(args, "-filter_complex")
	if !ok {
		t.Fatal("no -filter_complex on a branded encode")
	}
	if _, plain := argValue(args, "-vf"); plain {
		t.Error("-vf alongside -filter_complex")
	}
	if got, _ := argValue(args, "-map"); got != "[out]" {
		t.Errorf("-map = %q, want [out]", got)
	}

	// 34% of 720 is 245, inset by 2.5% of 720, which is 18.
	if !strings.Contains(chain, "scale=-1:245") {
		t.Errorf("chain = %q, want the logo scaled to 245px tall", chain)
	}
	if !strings.Contains(chain, "overlay=18:18") {
		t.Errorf("chain = %q, want the logo inset 18px from the top left", chain)
	}
	// -1 preserves the crest's own aspect; a fixed width would squash it.
	if strings.Contains(chain, "scale=0:245") {
		t.Errorf("chain = %q, want the logo width left to the aspect ratio", chain)
	}
}

func TestBrandedEncodeStillFeedsTheLogoAsASecondInput(t *testing.T) {
	args := (&Encoder{Logo: "/tmp/crest.png"}).encodeArgs("list.txt", "out.mp4", 720)

	var inputs int
	for i, a := range args {
		if a == "-i" && i+1 < len(args) {
			inputs++
		}
	}
	if inputs != 2 {
		t.Errorf("got %d inputs, want the frame list and the logo", inputs)
	}
}

// yuv420p cannot encode an odd height, so the scale has to land on an even one —
// and the logo size is derived from that height, so getting it wrong misplaces
// the branding as well as breaking the encode.
func TestScaledHeightIsAlwaysEven(t *testing.T) {
	for _, tc := range []struct {
		w, sw, sh, want int
	}{
		{1280, 3840, 2160, 720},
		{480, 3840, 2160, 270},
		{1280, 1920, 1080, 720},
		{333, 3840, 2160, 186}, // 187.3 rounds to 187, then down to 186
	} {
		got := scaledHeight(tc.w, tc.sw, tc.sh)
		if got != tc.want {
			t.Errorf("scaledHeight(%d, %d, %d) = %d, want %d", tc.w, tc.sw, tc.sh, got, tc.want)
		}
		if got%2 != 0 {
			t.Errorf("scaledHeight(%d, %d, %d) = %d, which is odd", tc.w, tc.sw, tc.sh, got)
		}
	}
}

func TestScaledHeightSurvivesADegenerateSource(t *testing.T) {
	if got := scaledHeight(1280, 0, 0); got != 0 {
		t.Errorf("scaledHeight with a zero-width source = %d, want 0", got)
	}
}
