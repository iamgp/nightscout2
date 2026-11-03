# syntax=docker/dockerfile:1
# Go cross-compiles for the target platform, so multi-arch images build
# natively without emulation.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/tideline ./cmd/tideline \
 && mkdir -p /out/data

# Litestream provides continuous backups (enabled with TIDELINE_BACKUP_URL).
# Built from source at a pinned release; the Go checksum database verifies it.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS litestream
ARG TARGETOS TARGETARCH
ARG LITESTREAM_VERSION=v0.5.18
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go install -trimpath -ldflags="-s -w" github.com/benbjohnson/litestream/cmd/litestream@${LITESTREAM_VERSION} \
 && mkdir -p /out \
 && cp "$(go env GOPATH)/bin/$( [ "$TARGETOS/$TARGETARCH" = "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" ] && echo litestream || echo "${TARGETOS}_${TARGETARCH}/litestream")" /out/litestream

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/tideline /tideline
COPY --from=litestream /out/litestream /usr/local/bin/litestream
COPY --from=build --chown=65532:65532 /out/data /data
# The container starts as root only so Tideline can take ownership of a
# host-mounted volume (Fly.io and Railway mount them as root). It then drops
# to this unprivileged user before opening the database or the network.
ENV TIDELINE_DB=/data/tideline.db PORT=8080 TIDELINE_RUN_AS=65532:65532
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/tideline"]
