# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/tideline ./cmd/tideline \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tideline /tideline
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV TIDELINE_DB=/data/tideline.db PORT=8080
VOLUME /data
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/tideline"]
