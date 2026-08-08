# towercam

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

Three artifacts per cycle:

| File | Contents |
| --- | --- |
| `latest.jpg` | Branded, overlaid — the public frame |
| `latest-clean.jpg` | Unbranded, for outlets applying their own graphics |
| `archive/YYYY/MM/DD/HHMMSS.jpg` | Clean master, for timelapses later |

All writes go temp-file → `rename`, so a web server never serves a torn frame.

## Setup

Drop three files in `assets/` (or point the env vars elsewhere):

- `font.ttf`, `font-bold.ttf` — a condensed grotesque reads best in a lower
  third. Inter, Barlow Condensed and Roboto Condensed all work.
- `logo.png` — transparent PNG, scaled automatically to the bar height.

```
go mod tidy
go build ./cmd/towercam
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
| `NWS_USER_AGENT` | `towercam` | api.weather.gov requires identification |
| `SITE_NAME` | *(unset)* | Rendered next to the logo |
| `SITE_ELEVATION_M` | `0` | Required for correct pressure — see below |
| `THEME_PATH` | *(unset)* | Layout JSON produced by the preview harness |
| `TZ` | `America/Los_Angeles` | Timestamp display |
| `OUTPUT_DIR` | `/var/www/towercam` | |
| `ARCHIVE_DIR` | *(unset)* | Unset disables archiving |
| `RETENTION_DAYS` | `0` | `0` keeps archives forever |
| `JPEG_QUALITY` | `92` | |
| `POLL_INTERVAL` | `15s` | How often Influx is asked for a newer observation |
| `MIN_FRAME_GAP` | `55s` | Floor between frames |
| `MAX_FRAME_AGE` | `3m` | Render anyway after this long |
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
  the altimeter reduction in `wx.PressureInHg` handles it.
- **`_time` is the observation time**, taken from the packet at second
  precision — not ingest time. That's what makes it trustworthy as a freshness
  signal, and why the Flux `range()` window doubles as the staleness gate: a
  silent station returns zero rows by construction.

If you enabled `RAPID_WIND` without a separate bucket, `rapid_wind_*` lands in
the same measurement — hence the explicit field allowlist in `wx/influx.go`.

## Serving

Point nginx or Caddy at `OUTPUT_DIR` with a short `max-age` and correct
`Last-Modified`, and give the newsroom one stable URL to poll.

## Not yet wired up

- Embedding assets with `go:embed` — currently loaded from disk, which is
  easier while you're iterating on the layout.
- Night handling. A tower cam at 3am is mostly noise; you may want to skip
  publishing, or swap in a darker overlay treatment, below some solar/lux
  threshold. `solar_radiation` and `illuminance` are already in Influx.
