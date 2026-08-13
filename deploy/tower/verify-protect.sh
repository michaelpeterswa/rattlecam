#!/usr/bin/env bash
# Checks a UniFi Protect console before pointing the daemon at it.
#
# Run this from somewhere that can actually reach the console — the tower host
# itself, or anything on that LAN. It reads the API key from the environment or
# from .env and never prints it.
#
#   ./verify-protect.sh
#   PROTECT_HOST=10.1.0.1 PROTECT_API_KEY=… PROTECT_CAMERA_ID=… ./verify-protect.sh
set -euo pipefail

cd "$(dirname "$0")"

if [ -f .env ]; then
  # shellcheck disable=SC1091
  set -a && . ./.env && set +a
fi

: "${PROTECT_HOST:?set PROTECT_HOST, e.g. 10.1.0.1}"
: "${PROTECT_API_KEY:?set PROTECT_API_KEY (or put it in .env)}"
: "${PROTECT_CAMERA_ID:?set PROTECT_CAMERA_ID}"

API="https://${PROTECT_HOST}/proxy/protect/integration/v1"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail() { printf '  FAIL  %s\n' "$*"; exit 1; }
ok() { printf '  ok    %s\n' "$*"; }

echo "Console ${PROTECT_HOST}, camera ${PROTECT_CAMERA_ID}"
echo

echo "1. Reachable"
if ! nc -z -w 5 "$PROTECT_HOST" 443 2>/dev/null; then
  fail "cannot open ${PROTECT_HOST}:443 — wrong address, or this host has no route to it"
fi
ok "port 443 open"

echo
echo "2. Certificate"
# Captured here rather than trusted blindly: pinning it is what stops a device
# swap on the LAN from silently substituting the image feeding the frame.
cert="$(echo | openssl s_client -connect "${PROTECT_HOST}:443" 2>/dev/null || true)"
[ -n "$cert" ] || fail "no TLS handshake"
fingerprint="$(printf '%s' "$cert" | openssl x509 -noout -fingerprint -sha256 2>/dev/null |
  cut -d= -f2 | tr -d ':' | tr '[:upper:]' '[:lower:]')"
[ -n "$fingerprint" ] || fail "could not read the leaf certificate"
ok "leaf sha256 captured"

echo
echo "3. Credentials and camera id"
code="$(curl -sk -o "$tmp/cam.json" -w '%{http_code}' \
  -H "X-API-KEY: ${PROTECT_API_KEY}" "${API}/cameras/${PROTECT_CAMERA_ID}")"
case "$code" in
  200) ok "camera found" ;;
  401 | 403) fail "$code — the API key was rejected (UniFi console -> Integrations)" ;;
  404) fail "404 — no camera with id ${PROTECT_CAMERA_ID} on this console" ;;
  *) fail "$code from the camera endpoint" ;;
esac
if command -v python3 >/dev/null; then
  python3 - "$tmp/cam.json" <<'PY' || true
import json, sys
try:
    d = json.load(open(sys.argv[1]))
    print(f"        name: {d.get('name', '?')}   state: {d.get('state', '?')}")
except Exception:
    pass
PY
fi

echo
echo "4. Snapshot"
code="$(curl -sk -o "$tmp/frame.jpg" -w '%{http_code}' \
  -H "X-API-KEY: ${PROTECT_API_KEY}" \
  "${API}/cameras/${PROTECT_CAMERA_ID}/snapshot?highQuality=true")"
[ "$code" = "200" ] || fail "$code from the snapshot endpoint"

# A 200 carrying JSON or HTML would decode to nothing useful, so check the bytes
# rather than the status. FFD8FF is the JPEG start-of-image marker.
magic="$(od -An -tx1 -N3 "$tmp/frame.jpg" | tr -d ' \n')"
[ "$magic" = "ffd8ff" ] || fail "response is not a JPEG (starts ${magic}, expected ffd8ff)"
bytes="$(wc -c <"$tmp/frame.jpg" | tr -d ' ')"
ok "JPEG, ${bytes} bytes"

if command -v sips >/dev/null; then
  dims="$(sips -g pixelWidth -g pixelHeight "$tmp/frame.jpg" 2>/dev/null |
    awk '/pixel/ {printf "%s ", $2}')"
  [ -n "$dims" ] && ok "dimensions: ${dims}"
fi

echo
echo "5. Certificate pinning"
# Verifies the pin is usable, i.e. that the certificate does not change between
# connections. A console behind a load balancer or re-issuing per connection
# would make pinning fail intermittently, which is worse than not pinning.
second="$(echo | openssl s_client -connect "${PROTECT_HOST}:443" 2>/dev/null |
  openssl x509 -noout -fingerprint -sha256 2>/dev/null |
  cut -d= -f2 | tr -d ':' | tr '[:upper:]' '[:lower:]')"
[ "$fingerprint" = "$second" ] || fail "the certificate changed between connections; pinning would fail intermittently"
ok "stable across connections"

echo
echo "All checks passed. Add to .env:"
echo
echo "  PROTECT_HOST=${PROTECT_HOST}"
echo "  PROTECT_CAMERA_ID=${PROTECT_CAMERA_ID}"
echo "  PROTECT_CERT_SHA256=${fingerprint}"
echo
echo "Leaving PROTECT_CERT_SHA256 unset disables verification entirely, which is"
echo "fine on a bench and less fine for something feeding a newsroom."
