package timelapse

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// colourFrame writes a JPEG with real chroma in it, which is what the camera
// sends with its IR-cut filter in place.
func colourFrame(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := range 48 {
		for x := range 64 {
			img.Set(x, y, color.RGBA{R: uint8(200 - x), G: uint8(60 + y), B: 40, A: 255})
		}
	}
	writeJPEG(t, path, img)
}

// monoFrame writes a greyscale JPEG, which is what the camera sends after the
// filter swings out.
func monoFrame(t *testing.T, path string) {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 64, 48))
	for y := range 48 {
		for x := range 64 {
			img.SetGray(x, y, color.Gray{Y: uint8((x + y) % 90)})
		}
	}
	writeJPEG(t, path, img)
}

func writeJPEG(t *testing.T, path string, img image.Image) {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encoding the fixture: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
}

func TestDaylightDropsTheFramesTheCameraTookInInfrared(t *testing.T) {
	dir := t.TempDir()
	day := filepath.Join(dir, "120000.jpg")
	night := filepath.Join(dir, "030000.jpg")
	colourFrame(t, day)
	monoFrame(t, night)

	sel, err := Daylight(context.Background(), []string{night, day}, 2)
	if err != nil {
		t.Fatalf("Daylight: %v", err)
	}

	if len(sel.Frames) != 1 || sel.Frames[0] != day {
		t.Errorf("kept %v, want just %s", sel.Frames, day)
	}
	if sel.Night != 1 {
		t.Errorf("Night = %d, want 1", sel.Night)
	}
}

// The frames go to ffmpeg in the order they come back, so a classifier that
// reordered them would produce a day that ran backwards in places.
func TestDaylightKeepsFramesInTheOrderGiven(t *testing.T) {
	dir := t.TempDir()

	var in []string
	var wantDay []string
	for i, name := range []string{"000000", "060000", "120000", "180000", "230000"} {
		p := filepath.Join(dir, name+".jpg")
		if i == 0 || i == 4 {
			monoFrame(t, p)
		} else {
			colourFrame(t, p)
			wantDay = append(wantDay, p)
		}
		in = append(in, p)
	}

	// Many workers, so any ordering bug has every chance to show.
	sel, err := Daylight(context.Background(), in, 8)
	if err != nil {
		t.Fatalf("Daylight: %v", err)
	}

	if len(sel.Frames) != len(wantDay) {
		t.Fatalf("kept %d frames, want %d", len(sel.Frames), len(wantDay))
	}
	for i := range wantDay {
		if sel.Frames[i] != wantDay[i] {
			t.Errorf("frame %d = %s, want %s", i, filepath.Base(sel.Frames[i]), filepath.Base(wantDay[i]))
		}
	}
}

// One truncated object in a bucket must not be able to fail a month.
func TestDaylightReportsAnUnreadableFrameRatherThanFailing(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "120000.jpg")
	bad := filepath.Join(dir, "121000.jpg")
	colourFrame(t, good)
	if err := os.WriteFile(bad, []byte("not a jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}

	sel, err := Daylight(context.Background(), []string{good, bad}, 2)
	if err != nil {
		t.Fatalf("Daylight: %v", err)
	}

	if len(sel.Frames) != 1 || sel.Frames[0] != good {
		t.Errorf("kept %v, want just the readable frame", sel.Frames)
	}
	if len(sel.Unreadable) != 1 || sel.Unreadable[0] != bad {
		t.Errorf("Unreadable = %v, want [%s]", sel.Unreadable, bad)
	}
}

func TestDaylightTreatsAMissingFileAsUnreadable(t *testing.T) {
	sel, err := Daylight(context.Background(), []string{filepath.Join(t.TempDir(), "gone.jpg")}, 1)
	if err != nil {
		t.Fatalf("Daylight: %v", err)
	}
	if len(sel.Unreadable) != 1 {
		t.Errorf("Unreadable = %v, want the missing file", sel.Unreadable)
	}
}

func TestDaylightOnNoFrames(t *testing.T) {
	sel, err := Daylight(context.Background(), nil, 4)
	if err != nil {
		t.Fatalf("Daylight: %v", err)
	}
	if len(sel.Frames) != 0 || sel.Night != 0 || len(sel.Unreadable) != 0 {
		t.Errorf("got %+v, want an empty selection", sel)
	}
}

func TestDaylightStopsOnACancelledContext(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "120000.jpg")
	colourFrame(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Daylight(ctx, []string{p}, 1); err == nil {
		t.Fatal("classifying with a cancelled context succeeded")
	}
}
