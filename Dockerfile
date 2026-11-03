# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/tideline ./cmd/tideline \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/tideline /tideline
COPY --from=build --chown=65532:65532 /out/data /data
# The container starts as root only so Tideline can take ownership of a
# host-mounted volume (Fly.io and Railway mount them as root). It then drops
# to this unprivileged user before opening the database or the network.
ENV TIDELINE_DB=/data/tideline.db PORT=8080 TIDELINE_RUN_AS=65532:65532
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/tideline"]
