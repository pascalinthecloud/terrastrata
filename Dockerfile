# syntax=docker/dockerfile:1

# ---- build stage ------------------------------------------------------------
FROM golang:1.26-alpine AS build

# Build metadata, supplied by `make docker` / CI. DATE may be passed for a
# reproducible build stamp; if omitted it falls back to the build time.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=

WORKDIR /src

# Download modules first so this layer caches independently of source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# Static, stripped, reproducible binary. CGO is off so it runs on distroless.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" \
    -o /out/terrastrata ./cmd/terrastrata

# Pre-create the cache directory with nonroot ownership; distroless has no shell
# to mkdir at runtime.
RUN mkdir -p /out/cache && chown -R 65532:65532 /out/cache

# ---- runtime stage ----------------------------------------------------------
# distroless static: no shell, no package manager — minimal attack surface.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/terrastrata /usr/local/bin/terrastrata
COPY --from=build --chown=65532:65532 /out/cache /cache

USER 65532:65532
EXPOSE 8080
ENV CACHE_DIR=/cache LISTEN_ADDR=:8080

ENTRYPOINT ["/usr/local/bin/terrastrata"]
