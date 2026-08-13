# assets

Branding, read from disk at runtime rather than compiled in, so the logo or a
font can change without rebuilding and redeploying the daemon.

The fonts are committed. Barlow Condensed is licensed under the SIL Open Font
License, which permits redistribution, so a clean checkout renders and a
CI-built image is self-contained — see `OFL-BarlowCondensed.txt`.

The artwork is not committed and is placed on the host by hand. The logo and the
peak annotation belong to the agency, and a public repository is the wrong place
for them.

Nothing renders until the fonts and, if you want a mark, the logo exist.

| File | What it should be |
| --- | --- |
| `font.ttf` | **Committed.** Barlow Condensed Regular. A condensed grotesque reads best in a lower third; Inter and Roboto Condensed also work and are likewise OFL. |
| `font-bold.ttf` | **Committed.** Barlow Condensed Bold. Values and the site name use it. |
| `logo.png` | *Not committed.* Transparent PNG. See the note on shape below. |
| `annotation.png` | *Not committed, optional.* Full-frame registered overlay — peak outlines and names. Must match the camera's aspect ratio. |

## Logo shape decides placement

The bar's logo slot scales the mark to the bar height, which suits a **wide
horizontal lockup**. A **tall portrait crest** cannot work there: at a bar height
of 11.5% it ends up a couple of hundred pixels wide at 4K, and any text inside it
turns to mush.

Set `logo_placement` in the theme accordingly:

| Value | Behaviour |
| --- | --- |
| `bar` | In the lower third, scaled to bar height. For horizontal lockups. |
| `top-left`, `top-right`, `bottom-left`, `bottom-right` | Corner badge, sized by `logo_height` as a fraction of image height. For portrait crests. |
| `none` | Omit the mark. |

A mostly-dark mark on a corner badge will lose its silhouette against a night
sky, and a mostly-light one will vanish against snow or blown-out cloud. Render
both `night` and a bright real still through the harness before committing.

Must be TrueType (`.ttf`). The renderer parses these directly and will not accept
`.otf` or a `.ttc` collection.

Replacing a font is a layout change, not a swap: the data columns are measured
from real glyph widths, so a wider face can push the widest readings back into
collision. Re-render `-contact` after changing one.

Override the locations with `FONT_PATH`, `BOLD_FONT_PATH` and `LOGO_PATH`, or
with `-font`, `-bold` and `-logo` on the preview harness. Passing an empty
`-logo` omits the mark entirely.

`Dockerfile` copies this directory into the image because it is read at runtime.

## Source stills for the harness

`cmd/preview` composites against a still you supply with `-image`; it does not
ship one. Pull a frame off the camera, or keep a few representative ones —
daylight, dusk, snow, blown-out sunrise — under `testdata/` and render against
each before settling the theme.
