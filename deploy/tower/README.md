# Tower deployment

The manifest for the box on the mountain — the one that can reach the camera and
the weather station. The compose file at the repository root is for local work;
it builds from source and brings up a Grafana stack. This one pulls a pinned
image and builds nothing.

## What has to be on the host

```
deploy/tower/
├── docker-compose.yml
├── .env                        from .env.example
├── theme.json                  copied from the repository root
├── assets/
│   ├── font.ttf                comes with the repository (OFL)
│   ├── font-bold.ttf           comes with the repository (OFL)

│   ├── logo.png
│   └── annotation.png          optional, must match the camera's aspect ratio
└── secrets/
    └── gcp-credentials.json    tofu output -raw rattlecam_service_account_key | base64 -d
```

The fonts come with the repository. Only the artwork is copied by hand — the
logo and the peak annotation are the agency's own and are not committed. The
daemon refuses to start without the fonts, which is deliberate: a missing font is
a startup failure, not a degraded frame.

Before filling in `.env`, check the console from a host that can reach it:

```sh
PROTECT_HOST=… PROTECT_API_KEY=… PROTECT_CAMERA_ID=… ./verify-protect.sh
INFLUX_URL=…    INFLUX_TOKEN=…    INFLUX_ORG=…       ./verify-influx.sh
```

`verify-protect.sh` confirms the address is routable, the key is accepted, the
camera id exists, and that the snapshot really is a decodable JPEG rather than a
200 carrying an error page. It prints the certificate fingerprint for
`PROTECT_CERT_SHA256`.

`verify-influx.sh` runs the exact query the daemon runs — same field allowlist,
same window — and reports what came back. Flux returns a bad query as a **200
with an error table** rather than a non-200, so a status code alone does not tell
you it worked. It also flags an observation older than `STALE_AFTER`, which is
the state where frames publish with no weather on them at all, and checks that
`p` looks like millibars rather than some other unit.

Neither script prints its credential.

### Dry run

Once `.env` is filled in and the assets are in place, prove the whole thing works
before anything is published:

```sh
./dry-run.sh
```

It runs the real daemon, from the same image production will use, against the
real camera and weather station — with uploads forced off, so the bucket and
everything the public reads are untouched. It checks a frame actually comes out,
that the branded and clean frames differ (so the overlay really drew), and how
many weather fields reached it.

Then **look at `dry-run-output/latest.jpg`**. Check the pressure against a local
forecast: if it reads several inches low, `SITE_ELEVATION_M` is wrong, and
nothing else will tell you.

## Going live

```sh
cp .env.example .env      # then fill it in
chmod 600 .env secrets/gcp-credentials.json
docker compose up -d
docker compose logs -f
```

A healthy start looks like:

```
INFO  publishing to object store  bucket=… archive=true
INFO  queueing uploads            spool=/spool max_bytes=2147483648
INFO  startup render check passed
INFO  rattlecam started           poll=15s output=/output
```

The render check draws one real frame before the loop begins, so a broken theme,
a missing font or an annotation authored at the wrong aspect ratio fails here
rather than on every frame while the process looks healthy.

## Why the spool is a named volume

It is what carries frames across a network outage: the daemon keeps working
through the outage and catches up afterwards. Losing it on a restart would
defeat the point, so it must not be a container path. `latest.jpg` supersedes
while queued and the archive accumulates, so recovery restores the live frame
first and then backfills history.

## Watching it

`METRICS_PORT` serves `/metrics` on loopback. The one that matters is
`rattlecam_observation_age_seconds` — everything else can look perfect while it
climbs, because the camera keeps returning stills and the frames keep publishing
with hours-old weather burned into them. Pair it with `rattlecam_frame_fields`:
age climbing with fields at zero is the staleness gate working correctly.

At roughly 1.1 MB a frame — the camera's own bytes, passed through rather than
re-encoded — the archive grows about 1.6 GB a day, and the default 2 GiB spool
holds around 33 hours of backlog before the oldest archive frames are dropped.

`rattlecam_spool_entries` says whether the link is keeping up. A backlog that
climbs and never falls is a dead link; one that never empties is a link too slow
to carry a frame a minute.

## Upgrading

```sh
sed -i 's/^RATTLECAM_VERSION=.*/RATTLECAM_VERSION=1.2.0/' .env
docker compose pull && docker compose up -d
```

The version is pinned rather than tracking `latest` on purpose: the link to this
site is slow and occasionally down, so a deploy should be a decision rather than
something that happens on its own the next time a container restarts.

## No healthcheck

The image is distroless and has no shell or `curl`, so there is nothing for a
compose `healthcheck` to run. `restart: unless-stopped` covers a crash; a process
that is running but wedged shows up as `rattlecam_publish_age_seconds` climbing,
which is what to alert on.
