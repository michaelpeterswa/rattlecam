#!/usr/bin/env bash
# Checks InfluxDB before pointing the daemon at it.
#
# The annotated-CSV parser is the least-exercised code in the repository and has
# only ever seen synthetic fixtures, so this runs the exact query the daemon
# runs and reports what came back — including the things that would silently
# produce a frame with no weather on it.
#
# Run from somewhere that can reach InfluxDB. The token is read from the
# environment or .env and never printed.
#
#   ./verify-influx.sh
#   INFLUX_URL=http://10.1.0.10:8086 INFLUX_TOKEN=… INFLUX_ORG=… ./verify-influx.sh
set -euo pipefail

cd "$(dirname "$0")"

if [ -f .env ]; then
  # shellcheck disable=SC1091
  set -a && . ./.env && set +a
fi

: "${INFLUX_URL:?set INFLUX_URL, e.g. http://10.1.0.10:8086}"
: "${INFLUX_TOKEN:?set INFLUX_TOKEN (or put it in .env)}"
: "${INFLUX_ORG:?set INFLUX_ORG}"
INFLUX_BUCKET="${INFLUX_BUCKET:-weather}"
INFLUX_STATION="${INFLUX_STATION:-}"
STALE_AFTER="${STALE_AFTER:-10m}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() { printf '  FAIL  %s\n' "$*"; exit 1; }
warn() { printf '  WARN  %s\n' "$*"; }
ok() { printf '  ok    %s\n' "$*"; }

# Seconds, because Go renders a minute as "1m0s" and Flux's duration grammar
# rejects that — the daemon does the same conversion.
window_seconds() {
  case "$1" in
    *h) echo $(( ${1%h} * 3600 )) ;;
    *m) echo $(( ${1%m} * 60 )) ;;
    *s) echo "${1%s}" ;;
    *) echo 600 ;;
  esac
}
WINDOW="$(window_seconds "$STALE_AFTER")s"

echo "InfluxDB ${INFLUX_URL}, org ${INFLUX_ORG}, bucket ${INFLUX_BUCKET}, window ${WINDOW}"
echo

echo "1. Reachable and authenticated"
code="$(curl -s -o "$tmp/health" -w '%{http_code}' --max-time 10 "${INFLUX_URL}/health" || echo 000)"
[ "$code" = "200" ] || fail "GET /health returned ${code} — wrong address, or no route from this host"
ok "service healthy"

code="$(curl -s -o "$tmp/buckets" -w '%{http_code}' --max-time 10 \
  -H "Authorization: Token ${INFLUX_TOKEN}" \
  "${INFLUX_URL}/api/v2/buckets?name=${INFLUX_BUCKET}")"
case "$code" in
  200) ok "token accepted" ;;
  401) fail "401 — the token was rejected" ;;
  403) fail "403 — the token cannot read buckets in org ${INFLUX_ORG}" ;;
  *) fail "${code} from the buckets endpoint" ;;
esac

grep -q "\"name\":\"${INFLUX_BUCKET}\"" "$tmp/buckets" ||
  fail "no bucket named ${INFLUX_BUCKET} is visible to this token"
ok "bucket ${INFLUX_BUCKET} exists"

echo
echo "2. The query the daemon actually runs"
# Kept deliberately identical to internal/wx/influx.go, including the explicit
# field allowlist. A wildcard would pull in rapid_wind_* if RAPID_WIND is ever
# enabled without a separate bucket.
FIELDS='"temp", "humidity", "dew_point", "p", "wind_avg", "wind_gust", "wind_lull", "wind_direction", "uv", "solar_radiation", "illuminance", "precipitation", "precipitation_type", "strike_count", "strike_distance", "battery"'
station_filter=""
[ -n "$INFLUX_STATION" ] && station_filter="  |> filter(fn: (r) => r.station == \"${INFLUX_STATION}\")
"

cat >"$tmp/query.flux" <<FLUX
from(bucket: "${INFLUX_BUCKET}")
  |> range(start: -${WINDOW})
  |> filter(fn: (r) => r._measurement == "weather")
${station_filter}  |> filter(fn: (r) => contains(value: r._field, set: [${FIELDS}]))
  |> last()
FLUX

code="$(curl -s -o "$tmp/result.csv" -w '%{http_code}' --max-time 30 \
  -XPOST "${INFLUX_URL}/api/v2/query?org=${INFLUX_ORG}" \
  -H "Authorization: Token ${INFLUX_TOKEN}" \
  -H "Content-Type: application/vnd.flux" \
  -H "Accept: application/csv" \
  --data-binary @"$tmp/query.flux")"
[ "$code" = "200" ] || fail "${code} from the query endpoint: $(head -c 300 "$tmp/result.csv")"

# Flux reports a bad query as a 200 with an error table rather than a non-200,
# so the status alone does not tell you it worked.
if head -5 "$tmp/result.csv" | grep -q '^,error,reference'; then
  fail "query error: $(sed -n '4p' "$tmp/result.csv" | cut -d, -f2)"
fi
ok "query accepted"

echo
echo "3. What came back"
python3 - "$tmp/result.csv" "$WINDOW" <<'PY'
import csv, sys, datetime

path, window = sys.argv[1], sys.argv[2]
rows = list(csv.reader(open(path)))

idx = {}
fields, times = {}, []
for r in rows:
    if not r or r[0].startswith('#'):
        continue
    if '_value' in r:
        idx = {n: i for i, n in enumerate(r)}
        continue
    if not idx or all(not c.strip() for c in r):
        continue
    try:
        name = r[idx['_field']].strip()
        value = r[idx['_value']].strip()
        if not name or not value:
            continue
        fields[name] = float(value)
        if '_time' in idx:
            times.append(r[idx['_time']].strip())
    except (KeyError, IndexError, ValueError):
        continue

if not fields:
    print("  FAIL  no rows in the window — the station has been silent, so every")
    print("        frame would publish with no weather on it")
    sys.exit(1)

print(f"  ok    {len(fields)} fields parsed")

# These five are what actually reach the frame; the rest are collected but unused.
for f in ("temp", "humidity", "dew_point", "p", "wind_avg"):
    if f in fields:
        print(f"        {f:<16} {fields[f]}")
    else:
        print(f"  WARN  {f:<16} absent — that column will be missing from the frame")

if times:
    newest = max(times)
    try:
        t = datetime.datetime.fromisoformat(newest.replace('Z', '+00:00'))
        age = (datetime.datetime.now(datetime.timezone.utc) - t).total_seconds()
        print(f"  ok    newest observation {age:.0f}s old ({newest})")
        if age > 600:
            print("  WARN  older than the default STALE_AFTER of 10m; the staleness gate")
            print("        would drop every field")
    except ValueError:
        print(f"  WARN  could not parse _time {newest!r}")
else:
    print("  WARN  no _time column — freshness cannot be established")

# tempest-influxdb writes bare numbers, so even counts arrive as floats. Decoding
# them as ints would be wrong, and this is where that would show up.
for f in ("precipitation_type", "strike_count"):
    if f in fields:
        print(f"  ok    {f} is a float ({fields[f]}), as expected")

if "p" in fields and not (800 <= fields["p"] <= 1100):
    print(f"  WARN  p is {fields['p']}, outside the plausible millibar range —")
    print("        check the collector is not writing some other unit")
PY

echo
echo "All checks passed. Add to .env:"
echo
echo "  INFLUX_URL=${INFLUX_URL}"
echo "  INFLUX_ORG=${INFLUX_ORG}"
echo "  INFLUX_BUCKET=${INFLUX_BUCKET}"
[ -n "$INFLUX_STATION" ] && echo "  INFLUX_STATION=${INFLUX_STATION}"
echo
echo "Remember SITE_ELEVATION_M: 'p' is station pressure, and publishing it"
echo "unreduced shows a number well below any local forecast."
