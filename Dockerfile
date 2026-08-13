# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# cache on every build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off so the result is a static binary the distroless image can run, and
# -trimpath so the build is reproducible regardless of where it happened.
# The seed directories exist to give the runtime volumes an owner. Docker copies
# whatever ownership a path has in the image into a fresh named volume; if the
# path is absent it creates one owned by root, which a container running as a
# non-root user cannot write to.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/rattlecam \
    ./cmd/rattlecam \
    && mkdir -p /seed/output /seed/spool

FROM gcr.io/distroless/static-debian12:nonroot

# The base ships /usr/share/zoneinfo, so time.LoadLocation resolves TZ without
# vendoring tzdata into the binary. That matters because a timezone that fails
# to load is a hard startup error, not a degraded mode.

WORKDIR /app

COPY --from=build /out/rattlecam /usr/local/bin/rattlecam

# Assets and the theme are read at runtime rather than embedded, so branding and
# layout can change without rebuilding. These two lines are why: remove them and
# the image cannot render anything.
#
# Fonts and artwork are gitignored, so a build from a clean checkout copies only
# assets/README.md and produces an image that needs assets mounted over /app/assets.
# A build from a working tree that has them produces a self-contained image.
COPY assets/ /app/assets/
COPY theme.json /app/theme.json

# The frame and the upload queue are written at runtime, so these have to be
# owned by the user the process runs as before a volume is mounted over them.
COPY --from=build --chown=65532:65532 /seed/output /output
COPY --from=build --chown=65532:65532 /seed/spool /spool

# 65532 is distroless's "nonroot". Numeric rather than named so the host can
# resolve it — a name means nothing outside the image, and an orchestrator
# enforcing runAsNonRoot cannot verify one.
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/rattlecam"]
