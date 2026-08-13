# Luma fixtures

Four frames from the Valley View camera, downscaled to 320x180, standing in for
the 4K originals under `/testdata` — which the repo deliberately does not carry.

| fixture | mean luma | |
| --- | --- | --- |
| `day.jpg` | 124.9 | clear afternoon |
| `valley.jpg` | 107.0 | the dimmest daylight frame on hand |
| `smoke.jpg` | 144.9 | wildfire haze, the brightest |
| `night.jpg` | 10.6 | after dark |

Those are `MeanLuma`'s own figures. An image tool asked for the same thing will
disagree by a point or two — ImageMagick's grayscale is gamma-aware and weights
the channels differently — so measure with the package, not with `magick`, when
updating them.

They exist because the night thresholds are only defensible if real frames sit
either side of them, and a test that skips when the originals are absent is not a
test — CI rejects skipped tests for exactly that reason.

Two things make the substitution sound:

- **Mean luma survives downscaling.** Each fixture measures within 0.9 of its
  original, so the numbers being asserted are the real camera's numbers.
- **They are JPEGs, not PNGs.** `image/jpeg` decodes to `*image.YCbCr`, which is
  the fast path `MeanLuma` actually takes in production. A PNG fixture would test
  a branch the daemon never runs.

Regenerating, if the camera or its exposure ever changes enough to matter:

```sh
magick testdata/still-day.jpg   -resize 320x180! -quality 85 internal/light/testdata/day.jpg
magick testdata/valley-view.jpg -resize 320x180! -quality 85 internal/light/testdata/valley.jpg
magick testdata/smoke.png       -resize 320x180! -quality 85 internal/light/testdata/smoke.jpg
magick testdata/still-night.jpg -resize 320x180! -quality 85 internal/light/testdata/night.jpg
```

Update the expected values in `light_test.go` to match; the test asserts each
fixture is within 2 of its recorded figure, so a silent drift fails rather than
quietly hollowing out the threshold checks.
