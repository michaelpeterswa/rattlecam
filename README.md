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
| `archive/YYYY/MM/DD/HHMMSS.jpg` | Clean master, for timelapses later |

All writes go temp-file → `rename`, so a web server never serves a torn frame.

## Setup

Drop three files in `assets/` (or point the env vars elsewhere):

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
The single exception is a failure to build the logger itself (`LOG_LEVEL`,
`LOG_FORMAT`), which cannot be logged and so goes straight to stderr.

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
| `LOG_FORMAT` | `text` | `text` or `json` |
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

Only `/latest.jpg` and `/latest-clean.jpg` are reachable. The archive is
deliberately absent: it is a bulk-download surface and nothing about the public
feed needs it.

```
GATEWAY_ADDR     listen address                     (default :8080)
GCS_BUCKET       bucket to read                     (required)
GCS_PREFIX       key prefix inside the bucket
GATEWAY_REFRESH  how often to check for a new frame (default 10s)
GATEWAY_RATE     requests per minute per client     (default 120, 0 disables)
GATEWAY_BURST    burst allowance per client         (default 20)
CACHE_CONTROL    header sent with every frame
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
