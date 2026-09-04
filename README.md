# rattlecam

Composites a logo, weather data and conditions text onto stills pulled from a
UniFi Protect camera, for a public website and for hand-off to news outlets.

## Start here: the preview harness

Settle the layout before anything runs unattended. `cmd/preview` composites
against a still you supply and synthetic weather, using the exact same
rendering and field-selection code the daemon will.

```sh
go build ./cmd/preview

# a full theme file to edit
./preview -dump-theme theme.json

# live reload: edit theme.json, refresh the browser
./preview -image tower.jpg -site "Cougar Mountain" -theme theme.json -serve :8099

# every scenario to ./out
./preview -image tower.jpg -site "Cougar Mountain" -all

# stack every scenario's bar for side-by-side legibility comparison
./preview -image tower.jpg -contact out/contact.png
```

Layout lives in `theme.json`, not in Go constants, so the serve mode re-reads
it per request — no rebuild between tweaks. Every value is a ratio (bar height
as a fraction of image height, everything else as a fraction of bar height), so
a theme tuned at 1080p renders identically at 4K.

Data columns are sized from what they contain rather than by dividing the strip
by the number of fields, because `WNW 48 mph G76` is several times the width of
`62%` and equal columns collide the first time it blows. `col_gap` sets the
minimum gutter between them; `col_spread` decides what happens to the space left
over, from `1` (spread across the whole strip) to `0` (packed against the site
block), which is mostly a question of how you want `partial` to look. If the
fields genuinely cannot fit, trailing ones are dropped rather than overprinted.

The built-in scenarios in `internal/wx/synthetic.go` are the cases that break a
layout you tuned against one pleasant afternoon reading:

| Scenario | What it catches |
| --- | --- |
| `typical` | The happy path |
| `wide-values` | −17.8°C, gusts to 76 mph, "Thunderstorm in Vicinity Heavy Rain" — the widest plausible strings |
| `calm` | Gust suffix should vanish when there's no meaningful delta |
| `night` | Bar contrast against a black sky |
| `partial` | Sensor dropout; columns must reflow, not leave gaps |
| `stale` | Observation past threshold — every data field drops out |
| `offline` | Station silent; the image still publishes, bare |
| `no-conditions` | api.weather.gov unreachable; site name sits alone |
| `lightning` | A storm passing — the amber warning pill appears top-right; everything else is ordinary |
| `smoke` | Wildfire smoke — a "214 V. Unhealthy" index beside an ordinary station reading |
| `stale-air` | Station fresh but the air quality feed quiet — that one field drops, the rest stay |

Point `-scenarios` at your own JSON to render against a real observation you
pulled off the station.

Once the theme looks right, hand the same file to the daemon via `THEME_PATH`.

## How the daemon runs

```
InfluxDB (weather) ─┐
                    ├─► trigger ─► Protect snapshot ─► composite ─► atomic publish
NWS observations ───┘                                                    │
                                                                    archive (clean)
```

The loop polls Influx every 15s and renders only when the observation `_time`
advances, so the picture and the numbers burned into it are always within a few
seconds of each other. A floor (`MIN_FRAME_GAP`) stops it rendering faster than
the station reports; a ceiling (`MAX_FRAME_AGE`) forces a frame every few
minutes so a dead station or a dead Influx can't freeze the published image.

The clean master and the archive are the camera's original JPEG bytes, passed
through untouched. Decoding and re-encoding them would cost a generation of
quality and roughly double the size — measured at 1.09 MB from the camera against
2.1 MB re-encoded at quality 92 — which on a mountain-top link is paid twice,
once uploading and again for every archived frame kept. Only the branded frame is
encoded, because it has actually been drawn on.

Three artifacts per cycle:

| File | Contents |
| --- | --- |
| `latest.jpg` | Branded, overlaid — the public frame |
| `latest-clean.jpg` | Unbranded, for outlets applying their own graphics — the camera's own bytes, unmodified |
| `latest-web.jpg` | The branded frame narrowed to `WEB_WIDTH`, for websites. Not archived |
| `archive/YYYY/MM/DD/HHMMSS.jpg` | Clean master, which `cmd/timelapse` later turns into the day, week and month videos |

All writes go temp-file → `rename`, so a web server never serves a torn frame.

## Setup

Drop three files in `assets/` (or point the env vars elsewhere):

- `font-mono.ttf` — Chivo Mono Regular, the timelapse clock's face. Monospaced
  so the timestamp keeps one width as it ticks.
- `font.ttf`, `font-bold.ttf` — a condensed grotesque reads best in a lower
  third. Inter, Barlow Condensed and Roboto Condensed all work.
- `logo.png` — transparent PNG.

`ANNOTATION_PATH` is a full-frame layer registered to what the lens sees — the
peak outline and names. It is drawn first, so the bar, the badge and the credit
all sit on top of it. Because it is aligned to the view rather than merely
decorative, it must be authored at the camera's aspect ratio: stretched to the
wrong shape it would point "Mount Si" at a different mountain, so a mismatch
beyond a percent is a hard error rather than a silent stretch. It may be authored
at any resolution with that shape; it is scaled once per output size and cached.
`annotation_opacity` scales it, and values above 1 strengthen a faint file rather
than clipping.

When the station has heard lightning in the last hour, an amber pill with a
bolt appears in the top-right corner: `LIGHTNING · 5 strikes in the last hour ·
last 3 min ago · ~6 mi`. It comes from an hourly query of the station's
`strike_count` and `strike_distance`, so it needs nothing beyond the InfluxDB
settings, and it disappears on its own an hour after the last strike.
`warning_placement` in the theme moves it (`top-left`) or hides it (`none`).

`CREDIT` is a standing attribution line. `credit_placement` puts it either in the
bar under the timestamp (`"bar"`), where it is compact but easy to overlook, or
in its own box at the top of the frame (`"top-center"`). On a tower cam the upper
third is sky — the one region reliably free of terrain — so a line can sit there
permanently without ever covering the view. It gets a backing box because sky
runs from near-white at midday to black overnight and no single text colour
survives both; at night the box disappears into the dark and the text carries
itself.

Where the mark goes depends on its shape, and `logo_placement` decides. A wide
horizontal lockup belongs in the bar (`"bar"`), scaled to the bar height. A tall
portrait crest does not survive that — squeezed into an 11.5% lower third it
comes out a sliver a couple of hundred pixels wide with unreadable interior
text — so give it a corner instead (`"top-left"` and friends) and size it with
`logo_height`, a fraction of image height. `"none"` omits it.

```
go mod tidy
go build ./cmd/rattlecam
```

`cmd/timelapse` needs `ffmpeg` on `PATH`; nothing else here does.

### Getting the Protect credentials

The API key comes from your UniFi console under **Integrations → New API Key**.
The camera ID is the GUID in the Protect dashboard URL when you open that
camera's settings page.

For `PROTECT_CERT_SHA256`, capture the console's leaf fingerprint once:

```sh
echo | openssl s_client -connect "$PROTECT_HOST:443" 2>/dev/null \
  | openssl x509 -noout -fingerprint -sha256
```

Leaving it unset falls back to skipping TLS verification. That's fine on a
bench, less fine for something feeding a newsroom — a device swap on your LAN
could then silently substitute the image.

## Configuration

A variable that is unset (or blank) takes its default. A variable that is *set*
but unparseable is an error, and every problem is reported at once so one restart
fixes them all. This matters more than it looks: silently falling back to the
default would turn `SITE_ELEVATION_M="1,200"` into elevation `0`, which makes the
pressure reduction a no-op and publishes raw station pressure — a wrong number on
a public frame with nothing logged anywhere.

`STALE_AFTER` must be positive; zero would switch off the staleness gate
entirely, which is the one thing that must never happen unattended.

Failures are logged, including fatal ones — a bad environment comes out as a
structured list of problems rather than a sentence with separators buried in it.
The single exception is a failure to build the logger itself (`LOG_LEVEL`), which
cannot be logged and so goes straight to stderr.

Logs are JSON on stdout, with no setting that makes them anything else. The lines
carry fields worth querying — the luma behind a night transition, the age of the
observation on a frame — and a format that is only sometimes parseable is one no
collector can be pointed at with confidence.

The daemon then renders one real frame before entering its loop. Fonts, the
crest, the annotation's aspect and every placement value in the theme are only
exercised when a frame is drawn, so without that check a typo fails on every
frame while the process sits there looking healthy and the feed quietly stops
advancing.

| Variable | Default | Notes |
| --- | --- | --- |
| `PROTECT_HOST` | — | Console address, no scheme |
| `PROTECT_API_KEY` | — | From Integrations |
| `PROTECT_CAMERA_ID` | — | GUID from the dashboard URL |
| `PROTECT_CERT_SHA256` | *(unset)* | Hex SHA-256; unset disables verification |
| `INFLUX_URL` | `http://localhost:8086` | |
| `INFLUX_ORG` / `INFLUX_TOKEN` / `INFLUX_BUCKET` | — / — / `weather` | |
| `INFLUX_STATION` | *(unset)* | Tempest serial, e.g. `ST-00000512` |
| `NWS_STATION_ID` | *(unset)* | e.g. `KPAE`; unset hides the conditions line |
| `NWS_USER_AGENT` | `rattlecam` | api.weather.gov requires identification |
| `AQI_URL` | *(unset)* | Prefix [aqi-api](https://github.com/michaelpeterswa/aqi-api) publishes under, e.g. `https://storage.googleapis.com/<bucket>/aqi/v1`; unset hides the air quality field |
| `AQI_INTERVAL` | `5m` | How often to re-read the published index; it republishes every five minutes |
| `SITE_NAME` | *(unset)* | Rendered at the left of the bar |
| `ANNOTATION_PATH` | `assets/annotation.png` | Registered overlay, e.g. peak outlines; absent at the default path is fine |
| `CREDIT` | *(unset)* | Standing attribution, e.g. `This view is provided by RSVU`; placed by `credit_placement` |
| `SITE_ELEVATION_M` | `0` | Required for correct pressure — see below. The daemon warns once if the pressure implies altitude and this is still `0` |
| `THEME_PATH` | *(unset)* | Layout JSON produced by the preview harness |
| `TZ` | `America/Los_Angeles` | Timestamp display |
| `METRICS_ENABLED` | `true` | |
| `METRICS_EXPORTER` | `prometheus` | `prometheus`, `otlpgrpc` or `otlphttp` |
| `METRICS_PORT` | `8081` | Serves `/metrics` and `/healthcheck` |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `OUTPUT_DIR` | `/var/www/rattlecam` | Always written; the local copy is the fallback |
| `GCS_BUCKET` | *(unset)* | Unset disables uploading entirely |
| `GCS_PREFIX` | *(unset)* | Optional key prefix inside the bucket |
| `GCS_ARCHIVE` | `true` | Upload dated masters under `archive/` |
| `GCS_CACHE_CONTROL` | `no-cache, max-age=0, must-revalidate` | Applied to the `latest-*` objects |
| `SPOOL_DIR` | `/var/spool/rattlecam` | Queue for frames the link could not carry yet |
| `SPOOL_MAX_BYTES` | `2147483648` | Backlog cap; oldest archive frames are dropped past it |
| `ARCHIVE_DIR` | *(unset)* | Unset disables archiving |
| `RETENTION_DAYS` | `0` | `0` keeps archives forever |
| `JPEG_QUALITY` | `92` | Applies to the branded frame only; the clean master is passed through |
| `WEB_WIDTH` | `1280` | Width of the extra copy for websites; `0` disables it |
| `POLL_INTERVAL` | `15s` | How often Influx is asked for a newer observation |
| `MIN_FRAME_GAP` | `55s` | Floor between frames |
| `MAX_FRAME_AGE` | `3m` | Render anyway after this long. Checked once per poll, so a value below `POLL_INTERVAL` cannot fire and is clamped to it with a warning. |
| `STALE_AFTER` | `10m` | Also the Flux `range()` window |

## Notes on the data

`tempest-influxdb` writes measurement `weather`, tagged `station`, with fields
`temp`, `humidity`, `dew_point`, `p`, `wind_avg`, `wind_gust`, `wind_lull`,
`wind_direction`, `uv`, `solar_radiation`, `illuminance`, `precipitation`,
`precipitation_type`, `strike_count`, `strike_distance`, `battery`.

Three things that matter here:

- **All fields are float64.** The collector writes bare numbers with no `i`
  suffix, so even `precipitation_type` and `strike_count` are floats.
- **`p` is station pressure, not sea level.** Publishing it raw would show a
  number well below what any local forecast says. Set `SITE_ELEVATION_M` and
  the altimeter reduction in `wx.PressureInHg` handles it. At the Valley View
  site this is the difference between publishing 29.94 in and 26.57 in, and
  nothing else in the pipeline notices: the query succeeds, the field is
  present, the frame renders, and the number is simply wrong. The daemon
  therefore warns once when the pressure is too low to be a sea-level reading
  and the elevation is still zero.
- **`_time` is the observation time**, taken from the packet at second
  precision — not ingest time. That's what makes it trustworthy as a freshness
  signal, and why the Flux `range()` window doubles as the staleness gate: a
  silent station returns zero rows by construction.

If you enabled `RAPID_WIND` without a separate bucket, `rapid_wind_*` lands in
the same measurement — hence the explicit field allowlist in `wx/influx.go`.

## Running it in a container

```sh
docker build -t rattlecam .
```

The image is distroless and runs as `nonroot` (uid 65532), so `OUTPUT_DIR` has to
be a volume that user can write to. `docker-compose.yml` wires that up along with
the Grafana LGTM stack for local metrics.

Assets and `theme.json` are read at runtime rather than embedded, which gives two
build modes worth knowing apart:

- **Locally**, with fonts and artwork present, `COPY assets/` bakes them in and
  the image is self-contained.
- **In CI**, they are gitignored, so the published image ships with only
  `assets/README.md` and needs the real ones mounted over `/app/assets`. Without
  them it exits immediately with `overlay: font: stat assets/font.ttf: no such
  file or directory` — which is the right failure, but it is a startup failure,
  not a degraded mode.

`TZ` resolves inside the image: `gcr.io/distroless/static-debian12` ships
`/usr/share/zoneinfo`, so `tzdata` does not need vendoring into the binary. An
unknown zone still fails loudly at startup rather than silently falling back to
UTC.

## Development

```sh
make            # wire up the commit-msg hook and install commitlint
make test       # go test -race -shuffle=on ./...
make lint       # golangci-lint, yamllint, hadolint — everything CI runs
make image      # docker build
```

Commits must be conventional: semantic-release reads them to decide the next
version, so a malformed message does not merely look untidy — it produces no
release, and no release means `upload_image.yml` never fires. The `commit-msg`
hook and CI both enforce it.

CI installs a TrueType font before running tests. `internal/overlay` renders
through real font files and skips itself when none is available, and fonts are
gitignored — so without that step the suite would "pass" by not running. A
separate step fails the build if any test reports `--- SKIP`.

## Metrics

`METRICS_PORT` serves `/metrics` and `/healthcheck`. Set `METRICS_EXPORTER` to
`otlpgrpc` to push to a collector instead, configured through the standard
`OTEL_EXPORTER_OTLP_*` variables; `docker-compose.yml` points that at the
bundled Grafana LGTM stack.

| Metric | Meaning |
| --- | --- |
| `rattlecam_observation_age_seconds` | Age of the weather observation behind the published frame |
| `rattlecam_publish_age_seconds` | Time since a frame was last written |
| `rattlecam_frame_fields` | Weather fields on the last frame; `0` means the staleness gate dropped them all |
| `rattlecam_frames_published_total` | Frames written |
| `rattlecam_frame_errors_total` | Failed cycles, by `stage` |
| `rattlecam_influx_errors_total` | By `kind`: `no_data` (silent station) vs `query_failed` (a bug) |
| `rattlecam_nws_errors_total` | Failed conditions refreshes |
| `rattlecam_snapshot_duration_seconds` / `rattlecam_influx_duration_seconds` | Latency |

**`rattlecam_observation_age_seconds` is the one that matters.** Everything else
can look perfect while it climbs: the camera keeps returning stills, the frames
keep publishing on schedule, and the weather burned into them is hours old. It is
computed when scraped rather than recorded per frame, so it rises on its own
while the process sits there looking healthy, and it is seeded at startup so the
series exists even if Influx was never reachable — a threshold alert cannot fire
on a metric that was never emitted.

Pair it with `rattlecam_frame_fields`: age climbing with fields at zero is the
staleness gate working correctly, and age climbing with fields non-zero would
mean it is not.

## Publishing to object storage

The camera sits on a radio tower with a finite uplink, and how many people are
watching should not be the tower's problem. With `GCS_BUCKET` set, each frame is
pushed once to the bucket and readers pull from there, so upstream cost is one
upload per frame regardless of audience. It also means the tunnel or the
on-premise host going away leaves the last uploaded frame still serving.

Local writes continue either way. A failed upload is reported as an error
wrapping `publish.ErrStore`, which the daemon logs and counts on
`rattlecam_store_errors_total` before carrying on — the frame is on disk, and a
stale feed beats treating the whole cycle as lost.

### Surviving an outage

The link to a mountain-top camera is not reliable, and an outage without a queue
is simply lost footage: the frame renders, the upload fails, and that moment
never reaches the bucket. So uploads do not happen on the render path at all. A
frame is finished once it reaches local disk; a separate drainer moves it to the
bucket when the network allows, retrying and catching up on its own.

The two object families need opposite queue semantics, which is the part worth
knowing:

- **`latest.jpg` supersedes.** Only the newest pending frame is kept. Draining an
  hour of superseded frames in order would march the public image backwards
  through the outage before arriving at the present.
- **`archive/…` accumulates.** Every entry is a distinct moment under an
  immutable name, and a timelapse with holes is what the queue exists to prevent.

Recovery restores the live frame first, then backfills history in chronological
order. The queue is bounded by `SPOOL_MAX_BYTES`, because a tower disk fills in
well under a day at a frame a minute — past the cap the oldest archive frames are
dropped, and `latest.jpg` is never evicted. Watch `rattlecam_spool_entries`: a
backlog that climbs and never falls is a dead link, and one that never empties is
a link too slow to keep up.

Two details that are easy to get wrong:

- **`Cache-Control` is set on the object at upload**, not by a proxy. A bucket's
  default is `public, max-age=3600`, so a stable `latest.jpg` URL would serve an
  hour-old frame long after a newer one landed. Archived frames are written once
  under a timestamped name and never change, so those go the other way and cache
  for a year.
- **Uploading needs `storage.objects.delete` as well as `create`.** Replacing an
  object requires both, and `latest.jpg` is replaced every minute, so
  `roles/storage.objectCreator` alone fails on every write.

Authentication is Application Default Credentials: the metadata server on GCP,
or `GOOGLE_APPLICATION_CREDENTIALS` pointing at a key file on an on-premise host.

### Serving it on a page

Use a plain `<img>`, not an iframe, and point it at `latest-web.jpg`. The full
frame is 4K because outlets composite their own graphics onto it; a browser does
not need that:

| Width | Size |
| --- | --- |
| 3840 | 2170 kB |
| 1920 | 781 kB |
| **1280** | **345 kB** |

The frame carries an `ETag`, so a refresh that finds nothing new costs a 304 of a
few hundred bytes. **A cache-busting query string throws that away** — every
request becomes a full transfer, which at a hundred viewers a minute is the whole
reason the gateway exists, undone. Refresh by re-fetching the same URL:

```js
const res = await fetch('https://cam.example.com/latest-web.jpg', { cache: 'no-cache' });
img.src = URL.createObjectURL(await res.blob());
```

`cache: no-cache` still sends `If-None-Match`; it means "revalidate", not "do not
cache". Sixty seconds matches the publish cadence — polling faster only buys 304s.

## Timelapses

`cmd/timelapse` builds three videos a night from the archive, and puts them in
the bucket next to the frames.

| Object | Window | Frames | Runs for | Rebuilt |
| --- | --- | --- | --- | --- |
| `timelapse/latest-today.mp4` | midnight until now | grows to ~142 | up to 6s | every 30 min |
| `timelapse/latest-yesterday.mp4` | the last complete day | ~142 | 6s | nightly |
| `timelapse/latest-weekly.mp4` | last 7 days | ~1,000 | 41s | nightly |
| `timelapse/latest-monthly.mp4` | last 30 days | ~2,700 | 1:52 | nightly |

Each has an animated preview beside it — `latest-weekly.gif` — and each except
today is also written under a dated name, `timelapse/weekly/2026-08-26.mp4`,
which never changes once written and caches for a year.

### Night included, or not

Every product except the monthly is published both ways. **The unsuffixed name
carries the treatment you would actually want for that period**, and `-daylight`
names the alternative:

| | unsuffixed | also published |
| --- | --- | --- |
| `today`, `yesterday`, `weekly` | night included | `-daylight` |
| `monthly` | daylight only | — |

The default differs by period on purpose. Over a day you want the whole day: the
dark hours are a few seconds and the transitions through them are the good part.
Over a month you do not — the same black rectangle recurs for a third of the
running time, forty minutes of it, and nobody watches a month to see thirty
nights. A night-included month would also be 150 MB, so it is not published.

### Today is the one that cannot be cached

Everything else is stitched from segments. Today cannot be: a segment is written
once under a name that says which day it is and is never revisited, which is
exactly wrong for a window that grows every ten minutes. So today is encoded from
its frames on every run — one day's frames per run rather than per day, which is
the price of it being current — and gets no dated copy, because it is superseded
forty-eight times a day and has become the yesterday product by midnight.

It does not run on the tower. A month of masters is a couple of gigabytes to
read, the uplink has already carried every one of those frames once, and reading
them back to make a video would spend it a second time on data already sitting in
the bucket. The job runs in the bucket's own region instead, where that traffic
is free. `deploy/timelapse` is the Cloud Run job and the schedule.

### One day encoded once

A day is encoded into two segments — the whole day, and the daylight hours on
their own — which are kept in the bucket. The products are then stitched from
segments with `ffmpeg -c copy`, which copies the compressed stream without
decoding it. A month joins in about fifty milliseconds and loses nothing, because
no pixel is re-encoded. So a night encodes one day, not thirty.

A run that finds a segment missing builds it, and that is the entire recovery
story: the first run builds the window, a run after an outage builds the days it
missed, and no state lives anywhere but the bucket.

The catch is that segments can only be joined when they were encoded identically.
Join a 1280-wide segment to a 1920-wide one and the second half of the result
decodes to garbage, with nothing anywhere reporting an error. So the settings are
part of the name:

```
timelapse/segments/1280w-24fps-crf25/2026-08-26.mp4
```

Change the width, the rate or the quality and the next run cannot see the old
segments at all. It builds a fresh set under the new name, which costs one
rebuild of the window and cannot corrupt a month.

### Why the month drops the night

The same IR-cut signal the overlay uses — see [Night](#night) — decides it, read
back off the archived frame rather than recomputed from a clock. Measured over
three days here it is unambiguous: exactly two transitions a day, chroma stepping
from 11–15 straight to 0.00, about 89 of 142 frames in colour in late August.

Dropping them is about watchability, not storage. It takes 37% off the running
time and only about 14% off the bytes, because a black frame compresses to
almost nothing while a daylit ridge does not. What it removes is forty minutes
of identical dark rectangle from a video meant to show a season moving.

### Branding

The RSVU crest is composited into the top-left corner at `TIMELAPSE_LOGO_HEIGHT`
of the frame height, defaulting to the same `logo_height` and `logo_margin` the
theme uses — a viewer moving between the live frame and a timelapse should not
have to find the branding twice.

It is burned into the **segments**, not the finished products, which is what
keeps the weekly and the monthly as stream copies. The cost is that changing the
branding invalidates every segment, and that is handled the same way every other
setting change is: the logo is part of the fingerprint, so turning it on builds a
fresh set rather than stitching a month from a mixture of branded and unbranded
days with the crest flickering in and out at each day boundary.

The artwork is fetched from the bucket rather than baked into the image, for the
same reason the daemon reads its assets from disk: the logo is the agency's and
is not in the repository, so an image built by CI cannot contain it. Put it at
`assets/logo.png` in the bucket. Absent at that default path means "unbranded";
set `TIMELAPSE_LOGO` explicitly and it is a startup error if missing, because the
alternative is publishing unbranded video for weeks and nobody noticing.

### The clock in the corner

Each frame carries the time it was taken, top-right, opposite the crest.

It comes from the frame's own name. The archive files a master as `133510.jpg`
under a dated directory, already in the site's local zone, so the label needs no
EXIF, no conversion and no daylight-saving special case — the name *is* the wall
clock a viewer is reading.

**The minute is rounded up to the nearest ten.** The camera's frames land at
00:04:25, 00:14:25, 00:24:40 and so on, so the true times drift by seconds and
read as noise; rounding to the archive's own cadence makes the clock tick
instead. Each frame stands for the ten minutes ending at the label it carries.

One visible consequence, and it happens most days: the last frame of a day is
usually after 23:50, so it rounds forward into the next day and shows
`2026-08-27 00:00` at the end of the 26th's video. That is the honest label for
a frame standing for the ten minutes that end at midnight, but it does look odd
the first time.

**The clock is set in a monospaced face**, Chivo Mono, rather than the
condensed one the daemon draws the overlay in. Proportional digits change width
as the clock advances — a `1` is narrower than a `0` — so a box that hugs the
text twitches at its left edge, against open sky, every time a digit changes,
and a box that does not hug it has to stand at the widest label the format can
produce and show the slack on the right. Monospaced, every one of the sixteen
characters advances 0.6 of the type size whatever digit it is, so all labels
measure alike and the box is an exact fit that never moves.

The box is still drawn separately from the text rather than by `drawtext`'s own
`box=1`, and computed from that advance: 9.6 ems of text plus padding. The only
slack in it is a few percent for `drawtext` rounding hinted glyph advances to
whole pixels, and the text is centred in the box — by `drawtext`'s own `text_w`
and `text_h`, which is what was actually laid out — so that slack is split
evenly either side instead of pooling on the right. Centring is safe here only
because the face is monospaced: those measurements are the same on every frame,
so nothing slides inside a box that is not moving.

Because the face changes every stamped pixel, it is part of the segment
fingerprint alongside the type size — otherwise a month rebuilt across the
change would be stitched from segments in two different faces.

The label reaches ffmpeg as packet metadata on each frame in the concat list,
because it differs per frame and a filter argument is fixed for the whole run.
Two keys, not one: the concat demuxer ends a metadata value at the first space,
so `d=2026-08-26` and `t=13:40` travel separately and the space lives in the
drawtext template. A single combined value arrives truncated to the date, and
the clock silently never appears.

The face ships in the image rather than coming from the bucket, because unlike
the crest it is OFL and committed. `TIMELAPSE_FONT=""` turns the clock off, and
pointing it at a proportional face works but puts the twitch back.

### Previews for embedding

Each product gets a GIF, sized for dropping into a page as a plain `<img>`.

GIF has no interframe prediction worth the name, so **file size is set by the
frame count and almost nothing else** — which is why `TIMELAPSE_GIF_FRAMES` caps
it at 200 and the longer products are decimated to fit. A useful consequence: a
GIF is roughly the same size whatever window it covers, because it always holds
about the same number of pictures. A month becomes a fast flip through the month
rather than a faithful rendition of the video, which is the right trade for
something whose job is to catch the eye above the fold.

Measured at the defaults — 480 wide, 12 fps, 200 frames — they run 5 to 9 MB. A
GIF of the weekly is therefore *smaller* than the mp4 it previews, and a GIF of
today is larger; that is the frame cap doing its work at both ends.

### Size is set by frame count, not duration

At ten-minute spacing consecutive frames share almost nothing, so x264 has no
temporal redundancy to work with and every frame costs roughly a full still —
about 33 kB at 1280 and CRF 25. A month is therefore around 130 MB however long
it runs, and halving the frame rate halves the duration without saving a byte.

Turning the archive cadence up would make frames *cheaper*, not dearer: at a
minute apart they start to correlate and inter-frame prediction begins working.

```
GCS_BUCKET              bucket holding the archive          (required)
GCS_PREFIX              key prefix inside the bucket
TZ                      zone the archive's days are in      (default America/Los_Angeles)
FFMPEG                  ffmpeg binary                       (default ffmpeg, from PATH)
TIMELAPSE_FPS           output frame rate                   (default 24)
TIMELAPSE_WIDTH         output width in pixels              (default 1280)
TIMELAPSE_CRF           x264 quality, lower is larger       (default 25)
TIMELAPSE_PRESET        x264 preset                         (default slow)
TIMELAPSE_WEEK_DAYS     days in the weekly                  (default 7)
TIMELAPSE_MONTH_DAYS    days in the monthly                 (default 30)
TIMELAPSE_BUILD_BUDGET  segments one run may encode         (default 31)
TIMELAPSE_WORKERS       concurrent downloads and decodes    (default 8)
TIMELAPSE_WORKDIR       scratch space                       (default a temp directory)
TIMELAPSE_MODE          nightly | today                     (default nightly)
TIMELAPSE_LOGO          logo object in the bucket           (default assets/logo.png)
TIMELAPSE_LOGO_HEIGHT   fraction of frame height            (default 0.34, matching theme.json)
TIMELAPSE_LOGO_MARGIN   inset, fraction of frame height     (default 0.025)
TIMELAPSE_GIF_WIDTH     preview width in pixels             (default 480)
TIMELAPSE_GIF_FPS       preview playback rate               (default 12)
TIMELAPSE_GIF_FRAMES    preview frame cap                   (default 200)
TIMELAPSE_FONT          face for the timestamp; empty disables it
                        (default the committed Chivo Mono; monospaced on
                        purpose, see "The clock in the corner")
TIMELAPSE_STAMP_HEIGHT  type size, fraction of frame height (default 0.045)
TIMELAPSE_STAMP_MARGIN  inset from the top-right corner     (default 0.025)
```

`-mode today` rebuilds only the today products, from today's frames. It is
scheduled every thirty minutes; the nightly build runs `-mode nightly` and does
everything else.

`TZ` has to match the daemon's. The archive's day directories are named in the
site's local zone by whichever process wrote them, so a job resolving dates in
UTC would build a "day" that starts at five in the afternoon.

Build one day again by deleting its segments first — they are never rebuilt while
they exist, which is what keeps a night cheap:

```sh
gcloud storage rm "gs://BUCKET/timelapse/segments/*/2026-08-26*.mp4"
gcloud run jobs execute rattlecam-timelapse --region us-west1 \
  --args=-date=2026-08-26 --wait
```

## The gateway

`cmd/gateway` serves the published frames from a **private** bucket, so the
bucket is never exposed and every request passes through one place that can rate
limit and log. It runs next to the reverse proxy rather than on the tower, and
reads the bucket with that host's own credentials — no key file, just the
instance identity.

The other half of its job is arithmetic. A frame is a couple of megabytes and
changes once a minute; fetching it per request would mean a bucket read and a
full transfer per viewer. It holds the frame in memory and re-reads only when the
generation changes, so bucket cost is a function of time rather than of audience.
Frames are served with an `ETag` taken from the generation, so a poller checking
every ten seconds costs a few hundred bytes rather than two megabytes.

Only a fixed set of paths is reachable — the three frames and the three
timelapses, by name. The archive is deliberately absent, and so are the dated
videos: both are bulk-download surfaces, and serving a name rather than a listing
is what keeps this from being a way to walk the bucket.

```
/latest.jpg  /latest-clean.jpg  /latest-web.jpg

/latest-today.mp4      /latest-today-daylight.mp4
/latest-yesterday.mp4  /latest-yesterday-daylight.mp4
/latest-weekly.mp4     /latest-weekly-daylight.mp4
/latest-monthly.mp4
```

...and a `.gif` beside each of those seven. The routes are generated from the
same `timelapse.Layout` and the same `timelapse.Variants` matrix the job
publishes with, so the gateway cannot come to serve a set of objects that differs
from the set that exists.

`/` is a listing of all of it — every path, what it is, how big it is and when it
was last written, grouped by frames, videos and previews. It is rendered from the
same route table, so it cannot advertise something that is not served or omit
something that is; an object with no `Title` is served but not listed, which is
how something stays reachable without being published. Objects the relevant job
has not built yet are listed as "not built yet" rather than hidden, because "not
there yet" and "does not exist" are different answers.

The videos are held in memory like everything else here, and they are much larger
than a frame — the monthly alone runs to about 90 MB, and the full set is roughly
200 MB. That is the same trade the frames make, read once per build rather than
once per viewer, but it is real memory: the gateway needs a host with room for
it, or `TIMELAPSE_SERVE=false` on one without. They are served through `http.ServeContent`, so a `<video>`
element can seek — a scrubber needs `Range`, and a server that answers every
request with the whole file has one that does not scrub.

Until the nightly job has run once the three video objects do not exist. That is
expected rather than a fault, so it is logged at debug and the paths return 503;
a missing *frame* is still a warning, because that one means something.

```
GATEWAY_ADDR     listen address                     (default :8080)
GCS_BUCKET       bucket to read                     (required)
GCS_PREFIX       key prefix inside the bucket
GATEWAY_REFRESH  how often to check for a new frame (default 10s)
GATEWAY_RATE     requests per minute per client     (default 120, 0 disables)
GATEWAY_BURST    burst allowance per client         (default 20)
CACHE_CONTROL    header sent with every frame
TIMELAPSE_SERVE  also serve the timelapses          (default true)
```

A failure reaching the bucket leaves the cached frame in place; a hiccup should
not take the feed down. Rate limiting keys on `X-Forwarded-For` when present,
because behind a proxy every request otherwise appears to come from one address
and all clients would share a single bucket.

## Serving

Point nginx or Caddy at `OUTPUT_DIR` with a short `max-age` and correct
`Last-Modified`, and give the newsroom one stable URL to poll.

## Night

The annotation is black ink. Over a daylit ridge that reads cleanly; against a
night sky it disappears, taking with it the one element that could still tell a
viewer what they are looking at. So after dark it is drawn inverted, in white.

Night is decided from the frame itself rather than a clock or a sun-position
calculation, because the question is not "has the sun set" but "can black ink
still be seen" — and overcast, smoke, terrain shadow and the camera's own
exposure move that hours either side of sunset.

The signal is the camera's IR-cut filter. When it swings out the sensor stops
reporting colour, and a frame's mean chroma drops from double figures to exactly
zero between one poll and the next. That is the camera's own judgement that the
light has gone, it is a single unambiguous event, and it needs no calibration.

Brightness alone will not do it, and a real dusk shows why:

| | luma | chroma | |
| --- | --- | --- | --- |
| 21:00 | 64.1 | 11.3 | last colour frame |
| 21:10 | 66.3 | **0.0** | first greyscale frame — *brighter* than the one before |

Ten minutes apart, either side of the switch, and the later frame is the lighter
of the two. Nothing keyed on brightness could separate them. Luma across that
evening fell smoothly from 129 at midday to 41 at 22:00 with no step anywhere,
and its night value moves tens of points with moon and cloud — measured between
10 and 42 on different nights here.

So luma is the fallback, not the signal. It still gets a say whenever the answer
is not already mono, because a frame can be in colour and still be far too dark
for black ink — one of the fixtures is exactly that, a colour frame at 10.5 luma.
Either signal alone is enough to call it night; a frame must be both in colour
and above `NIGHT_EXIT_LUMA` to return to day.

| var | default | |
|---|---|---|
| `NIGHT_ENTER_LUMA` | `50` | at or below this, night begins even in colour |
| `NIGHT_EXIT_LUMA` | `75` | a colour frame at or above this restores day |
| `NIGHT_INVERT_ANNOTATION` | `true` | set false to keep black ink around the clock |

The two thresholds are a hysteresis band, and the gap between them matters
because luma is a slope: with a single threshold, a camera relying on the
fallback would flip on every poll through dusk. Startup fails if they are not
ordered.

Nothing else about the frame changes at night. The same live picture publishes on
the same cadence, the data bar already carries its own backing so it survives a
black background unaided, and masters are archived at the same interval around
the clock — a frame not archived tonight cannot be recovered tomorrow, and the
bucket's lifecycle rules already tier old masters down to nearline and colder,
which is the cheaper place to solve storage cost than by never writing them.

`rattlecam_night` and `rattlecam_frame_luma` expose the state and the brightness
behind it. The transition log line names which signal decided, so a threshold
problem reads differently from a camera problem.

To see the treatment without waiting for dark, point the preview harness at a
night capture — `-night` defaults to `auto` and measures the still exactly as the
daemon does:

```sh
go run ./cmd/preview -image testdata/still-night.jpg -scenario night -theme theme.json
go run ./cmd/preview -image testdata/still-night.jpg -scenario night -night off  # for comparison
```

## Not yet wired up

- Embedding assets with `go:embed` — currently loaded from disk, which is
  easier while you're iterating on the layout.
