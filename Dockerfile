# syntax=docker/dockerfile:1

FROM golang:1.26-bookworm AS build

WORKDIR /src

# Dependencies first, so a source-only change does not re-download the module
# cache on every build.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# CGO off so the result is a static binary the distroless image can run, and
# -trimpath so the build is reproducible regardless of where it happened.
#
# The two cache mounts are what make a rebuild quick. Layer caching alone only
# helps when nothing above a layer changed, which is never true of the layer that
# compiles the source you just edited — so without these, every build recompiles
# the whole dependency tree from scratch. Held across builds they turn a
# source-only change into a near-incremental compile: measured 9s to 3s here.
#
# They are BuildKit cache mounts, not layers, so nothing about them reaches the
# published image.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/rattlecam \
    ./cmd/rattlecam

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

# 65532 is distroless's "nonroot". Numeric rather than named so the host can
# resolve it — a name means nothing outside the image, and an orchestrator
# enforcing runAsNonRoot cannot verify one.
USER 65532:65532

ENTRYPOINT ["/usr/local/bin/rattlecam"]
