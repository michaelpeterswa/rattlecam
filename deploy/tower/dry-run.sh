#!/usr/bin/env bash
# Runs the real daemon against the real camera and weather station, publishing
# nowhere.
#
# The two verify scripts check each dependency on its own. This checks the thing
# that actually matters: that a frame comes out, with the right numbers on it,
# using the same image that will run in production. Uploads are forced off, so
# nothing reaches the bucket and nothing the public reads is touched.
#
#   ./dry-run.sh            # 90 seconds, writes to ./dry-run-output
#   ./dry-run.sh 300        # longer
set -euo pipefail

cd "$(dirname "$0")"

DURATION="${1:-90}"
OUT="$PWD/dry-run-output"

fail() { printf '  FAIL  %s\n' "$*"; exit 1; }
ok() { printf '  ok    %s\n' "$*"; }
warn() { printf '  WARN  %s\n' "$*"; }

command -v docker >/dev/null || fail "docker is not installed on this host"
[ -f .env ] || fail "no .env — copy .env.example and fill it in"

# shellcheck disable=SC1091
set -a && . ./.env && set +a

echo "Preflight"
: "${RATTLECAM_VERSION:?set RATTLECAM_VERSION in .env}"
for v in PROTECT_HOST PROTECT_API_KEY PROTECT_CAMERA_ID INFLUX_URL INFLUX_ORG INFLUX_TOKEN; do
  [ -n "${!v:-}" ] || fail "$v is empty in .env"
done
ok ".env has the required values"

[ -n "${PROTECT_CERT_SHA256:-}" ] || warn "PROTECT_CERT_SHA256 unset — the console's certificate will not be verified"

# The daemon refuses to start without these, which is deliberate, but failing
# here is faster than failing inside a container.
for f in assets/font.ttf assets/font-bold.ttf theme.json; do
  [ -f "$f" ] || fail "missing $f"
done
ok "fonts and theme present"
[ -f assets/logo.png ] || warn "no assets/logo.png — the frame will render without a mark"
[ -f assets/annotation.png ] || warn "no assets/annotation.png — no peak outline"

if [ "${SITE_ELEVATION_M:-0}" = "0" ]; then
  warn "SITE_ELEVATION_M is 0 — the frame will publish unreduced station pressure"
fi

rm -rf "$OUT" && mkdir -p "$OUT"

echo
echo "Running for ${DURATION}s, publishing nowhere"
# GCS_BUCKET empty disables uploading and the spool with it, so this cannot
# touch the bucket even if .env is fully configured for production.
docker run --rm --name rattlecam-dry-run \
  --env-file .env \
  -e GCS_BUCKET= \
  -e OUTPUT_DIR=/output \
  -e THEME_PATH=/app/theme.json \
  -e METRICS_ENABLED=false \
  -e MIN_FRAME_GAP=0s \
  -e POLL_INTERVAL=15s \
  -v "$PWD/assets:/app/assets:ro" \
  -v "$PWD/theme.json:/app/theme.json:ro" \
  -v "$OUT:/output" \
  "ghcr.io/michaelpeterswa/rattlecam:${RATTLECAM_VERSION}" \
  >"$OUT/daemon.log" 2>&1 &
pid=$!

# shellcheck disable=SC2064
trap "docker rm -f rattlecam-dry-run >/dev/null 2>&1 || true" EXIT

elapsed=0
while [ "$elapsed" -lt "$DURATION" ]; do
  if ! kill -0 "$pid" 2>/dev/null; then
    echo
    echo "The daemon exited early:"
    sed 's/^/    /' "$OUT/daemon.log" | tail -20
    exit 1
  fi
  [ -f "$OUT/latest.jpg" ] && break
  sleep 3
  elapsed=$((elapsed + 3))
done

echo
echo "Result"
sed 's/^/    /' "$OUT/daemon.log" | grep -E 'startup render|rattlecam started|published|WARN|ERROR' | head -12 || true

[ -f "$OUT/latest.jpg" ] || fail "no frame was produced in ${DURATION}s — see $OUT/daemon.log"

for f in latest.jpg latest-clean.jpg; do
  [ -f "$OUT/$f" ] || fail "$f was not written"
  bytes="$(wc -c <"$OUT/$f" | tr -d ' ')"
  magic="$(od -An -tx1 -N3 "$OUT/$f" | tr -d ' \n')"
  [ "$magic" = "ffd8ff" ] || fail "$f is not a JPEG"
  ok "$f  ${bytes} bytes"
done

# The clean master should be the camera's own bytes; the branded one is encoded
# because it has been drawn on, so they must differ.
if cmp -s "$OUT/latest.jpg" "$OUT/latest-clean.jpg"; then
  fail "latest.jpg and latest-clean.jpg are identical — the overlay did not draw"
fi
ok "the branded and clean frames differ, so the overlay drew"

fields="$(grep -o 'fields=[0-9]*' "$OUT/daemon.log" | tail -1 | cut -d= -f2)"
case "${fields:-0}" in
  0) warn "the frame published with zero weather fields — the reading was stale or absent" ;;
  *) ok "${fields} weather fields on the frame" ;;
esac

echo
echo "Look at ${OUT}/latest.jpg before deploying. Check the pressure against a"
echo "local forecast: if it reads several inches low, SITE_ELEVATION_M is wrong."
