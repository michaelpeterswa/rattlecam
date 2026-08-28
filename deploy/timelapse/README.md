# The nightly timelapse job

Builds three videos from the archive, once a night, next to the bucket.

| Product | Window | Frames | Runs for |
| --- | --- | --- | --- |
| `latest-daily.mp4` | yesterday | ~142 | 6s |
| `latest-weekly.mp4` | last 7 days | ~1,000 | 41s |
| `latest-monthly.mp4` | last 30 days, daylight only | ~2,700 | 1:52 |

Each is also written under a dated name — `timelapse/daily/2026-08-26.mp4` and
so on — which never changes once written and caches for a year.

## Why it does not run on the tower

A month of masters is a couple of gigabytes to read. The tower's uplink is the
one resource the whole design is arranged around, and it has already carried
every one of those frames once. Reading them back over it to make a video would
spend it a second time, for a job whose input is already sitting in the bucket.

So the job runs in the same region as the bucket, where that traffic is free and
crosses no radio link. The tower never knows this exists.

## How a night's work stays small

A day is encoded once into two segments — the whole day, and the daylight hours
on their own — and those are kept in the bucket. The products are then stitched
from segments with `ffmpeg -c copy`, which copies the compressed stream without
decoding it: a month joins in under a second and loses nothing, because no pixel
is re-encoded.

So an ordinary night encodes one day, not thirty.

```
archive/2026/08/26/*.jpg  ──encode──►  segments/2026-08-26.mp4           ──┐
                          ──encode──►  segments/2026-08-26-daylight.mp4  ─┐│
                                                                         ││
   segments for the last 7 days   ───────────────────copy───────────────► ││ weekly
   segments for the last 30 days (daylight) ─────────copy───────────────► │  monthly
   yesterday's segment ───────────────────────────────copy──────────────► ┘  daily
```

A run that finds a segment missing builds it. That is the whole of the recovery
story: the first run finds none and builds the window, a run after an outage
builds the days it missed, and there is no state anywhere but the bucket. It is
bounded by `TIMELAPSE_BUILD_BUDGET` so a first run cannot exceed the job's hour.

## Segments and settings

Segments can only be joined when they were encoded identically — same codec,
resolution and frame rate. Join a 1280-wide segment to a 1920-wide one and the
result is a file whose second half no player decodes correctly, with no error
anywhere to say so.

So the settings are part of the segment's name:

```
timelapse/segments/1280w-24fps-crf25/2026-08-26.mp4
```

Change the width, the rate or the quality and the next run simply cannot see the
old segments. It builds a fresh set under the new name and the stale ones age out
with the bucket's lifecycle rules. Changing a setting therefore costs one full
rebuild of the window, which is minutes, and cannot corrupt a month.

## Applying it

The bucket is not managed here — it belongs to the deployment that publishes the
frames, and this only asks for a role on it.

This is the parameterised form, kept beside the code so the two do not drift.
The deployed copy lives in the Rattlesnake Mountain infrastructure repository as
`rattlecam-timelapse.tf`, where the project and the bucket are concrete resources
rather than variables. Change one, change the other.

Two things the job needs before it can run: `run.googleapis.com` and
`cloudscheduler.googleapis.com` enabled on the project, and the image public on
GHCR — new packages default to private, and Cloud Run pulls it directly the same
way the tower pulls the daemon image.

One thing the bucket needs, wherever it is defined: a lifecycle rule expiring
`timelapse/weekly/` and `timelapse/monthly/` after thirty days. Those dated
copies are almost pure duplication — each night's weekly shares six of its seven
days with the night before — and without a rule they accumulate about 56 GB a
year, forever. Nothing is lost past thirty days, because any window can be
rebuilt from the masters on demand.

Mind the trailing slash. `timelapse/weekly` would also match
`timelapse/latest-weekly.mp4`, the stable name the gateway serves.

```sh
terraform init
terraform apply \
  -var project=rm-main-p-hj56 \
  -var bucket=rm-main-p-hj56-rattlecam \
  -var image=ghcr.io/michaelpeterswa/rattlecam-timelapse:1.2.0
```

Then run it once by hand rather than waiting for midnight:

```sh
gcloud run jobs execute rattlecam-timelapse --region us-west1 --wait
```

The first execution has no segments and builds the whole window — expect ten
minutes or so, and one line of log per day as it goes.

## Building one day again

Segments are never rebuilt once they exist, which is what keeps a night cheap. To
redo one — a day that encoded from a partial download, say — delete its segments
and run the job for that date:

```sh
gcloud storage rm "gs://BUCKET/timelapse/segments/*/2026-08-26*.mp4"
gcloud run jobs execute rattlecam-timelapse --region us-west1 \
  --args=-date=2026-08-26 --wait
```

## What to watch

The job logs JSON on stdout, the same shape as the daemon and the gateway.

- `msg="building segments"` carries `frames`, `daylight` and `night` for the day.
  On this site a complete day is 142 frames, of which about 89 are daylight in
  late August. A day well short of 142 means the archive has holes in it, which
  is a question for the tower rather than for this.
- `msg="no frames archived; the day is left out"` is a day the camera or the link
  was down. The products are built from the days that do exist.
- `msg="segment build budget reached"` means the window is further behind than
  one run can catch up on. It is self-correcting — the next run continues — but
  several nights in a row means something is wrong upstream.
