# Luma fixtures

Four frames from the Valley View camera, downscaled to 320x180, standing in for
the 4K originals under `/testdata` — which the repo deliberately does not carry.

| fixture | luma | chroma | |
| --- | --- | --- | --- |
| `day.jpg` | 124.9 | 33.5 | clear afternoon |
| `valley.jpg` | 107.0 | 48.0 | dimmest daylight on hand |
| `smoke.jpg` | 144.1 | 22.1 | wildfire haze, the brightest |
| `dusk-colour.jpg` | 64.2 | 11.3 | last colour frame before the IR switch |
| `dusk-mono.jpg` | 66.2 | 0.0 | first greyscale frame, ten minutes later |
| `night.jpg` | 10.5 | 6.9 | colour, but far too dark for black ink |

The last three are the ones that earn their place. `dusk-colour` and `dusk-mono`
sit either side of a real IR-cut switch ten minutes apart, and their luma is
*almost identical* — 64.2 then 66.2, the later frame being the brighter of the
two. Nothing keyed on brightness could separate them, which is why mono leads and
luma is the fallback. `night.jpg` is the opposite case: a colour frame at 10.5
luma, dark enough that black ink is invisible while the camera is still reporting
colour, which is why luma is kept at all.

Those figures are `MeanLuma` and `MeanChroma`'s own. An image tool asked for the
same thing will disagree by a point or two — ImageMagick's grayscale is
gamma-aware and weights the channels differently — so measure with the package,
not with `magick`, when updating them.

They exist because the thresholds are only defensible if real frames sit either
side of them, and a test that skips when the originals are absent is not a test —
CI rejects skipped tests for exactly that reason.

Three things make the substitution sound:

- **Mean luma survives downscaling.** Each fixture measures within 0.9 of its
  original, so the numbers being asserted are the real camera's numbers.
- **They are JPEGs, not PNGs.** `image/jpeg` decodes to `*image.YCbCr`, which is
  the path `MeanLuma` and `MeanChroma` actually take in production. A PNG fixture
  would test a branch the daemon never runs.
- **`dusk-mono.jpg` is forced to three channels** with `-type TrueColor`. Left to
  itself ImageMagick notices the chroma is redundant and emits a single-channel
  JPEG, which decodes to `*image.Gray` — still correctly detected as mono, but no
  longer the shape the camera actually sends.

Regenerating, if the camera or its exposure ever changes enough to matter:

```sh
magick <4k-still> -resize 320x180! -quality 85 internal/light/testdata/<name>.jpg
magick <4k-mono-still> -resize 320x180! -type TrueColor -quality 85 \
  internal/light/testdata/dusk-mono.jpg
```

Update the expected values in `light_test.go` to match; the test asserts each
fixture is within 2 of its recorded figure, so a silent drift fails rather than
quietly hollowing out the checks.
