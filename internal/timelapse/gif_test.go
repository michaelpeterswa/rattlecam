package timelapse

import (
	"strings"
	"testing"
)

// GIF stores something close to a whole picture per frame, so the frame cap is
// what actually bounds the file. A month is 2,700 frames and must not be
// rendered whole.
func TestDecimationFitsTheFrameCap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames int
		max    int
		want   int
	}{
		{"under the cap is untouched", 142, 200, 1},
		{"exactly the cap is untouched", 200, 200, 1},
		{"a week is thinned", 994, 200, 5},
		{"a month is thinned harder", 2700, 200, 14},
		{"no cap", 5000, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decimation(tc.frames, tc.max); got != tc.want {
				t.Errorf("decimation(%d, %d) = %d, want %d", tc.frames, tc.max, got, tc.want)
			}
		})
	}
}

// Whatever the step, the result has to come out at or under the cap — that is
// the entire promise the cap makes about file size.
func TestDecimationNeverLeavesMoreFramesThanTheCap(t *testing.T) {
	for _, frames := range []int{1, 199, 200, 201, 994, 2700, 4260} {
		for _, max := range []int{50, 120, 200} {
			step := decimation(frames, max)
			kept := (frames + step - 1) / step
			if kept > max {
				t.Errorf("%d frames at step %d keeps %d, which is over the cap of %d", frames, step, kept, max)
			}
		}
	}
}

// The filtergraph parser splits on commas before any filter sees its own
// arguments, so an unescaped comma inside mod() ends the select filter halfway
// through and ffmpeg rejects the whole chain.
func TestSelectChainEscapesTheCommaInsideTheExpression(t *testing.T) {
	got := GIFOptions{}.selectChain(14)

	if !strings.Contains(got, `mod(n\,14)`) {
		t.Errorf("chain = %q, want the comma escaped as \\,", got)
	}
	if strings.Contains(got, "mod(n,14)") {
		t.Errorf("chain = %q, has a bare comma inside the expression", got)
	}
}

// A step of one means every frame is kept, and a select that keeps everything is
// pure cost.
func TestSelectChainOmitsSelectWhenNothingIsDropped(t *testing.T) {
	if got := (GIFOptions{}).selectChain(1); strings.Contains(got, "select") {
		t.Errorf("chain = %q, want no select filter at step 1", got)
	}
}

func TestSelectChainSetsThePlaybackRateAndWidth(t *testing.T) {
	got := GIFOptions{Width: 320, FPS: 10}.selectChain(3)

	if !strings.Contains(got, "setpts=N/(10*TB)") {
		t.Errorf("chain = %q, want the timestamps restamped for 10 fps", got)
	}
	if !strings.Contains(got, "scale=320:-2") {
		t.Errorf("chain = %q, want a scale to 320", got)
	}
	// -2 rather than -1: an odd height is legal in a GIF but the scaler still
	// has to land somewhere, and evenness keeps it consistent with the mp4.
	if strings.Contains(got, "scale=320:-1") {
		t.Errorf("chain = %q, want the height rounded to even", got)
	}
}

func TestGIFOptionsFallBackToTheDefaults(t *testing.T) {
	o := GIFOptions{}
	if o.width() != DefaultGIFWidth || o.fps() != DefaultGIFFPS || o.maxFrames() != DefaultGIFMaxFrames {
		t.Errorf("zero options = %d/%d/%d, want the defaults %d/%d/%d",
			o.width(), o.fps(), o.maxFrames(), DefaultGIFWidth, DefaultGIFFPS, DefaultGIFMaxFrames)
	}
}
