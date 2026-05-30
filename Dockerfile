# --platform=$BUILDPLATFORM pins the build stage to the runner's native arch so the
# Go toolchain runs un-emulated; we cross-compile to TARGETARCH below.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=0 because modernc.org/sqlite is pure Go — gives us a static binary and
# lets us cross-compile for any TARGETARCH from the native build host (no QEMU needed,
# since the final stage only COPYs — it never RUNs target-arch code).
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/plex-mirror ./cmd/plex-mirror

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/plex-mirror /plex-mirror
EXPOSE 8080
ENV PLEXMIRROR_HTTP_ADDR=:8080 \
    PLEXMIRROR_DB_PATH=/var/lib/plex-mirror/state.db \
    PLEXMIRROR_MEDIA_ROOT=/media
USER nonroot:nonroot
ENTRYPOINT ["/plex-mirror"]
