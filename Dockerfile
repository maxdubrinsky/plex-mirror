FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=0 because modernc.org/sqlite is pure Go — gives us a static binary.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/plex-mirror ./cmd/plex-mirror

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/plex-mirror /plex-mirror
EXPOSE 8080
ENV PLEXMIRROR_HTTP_ADDR=:8080 \
    PLEXMIRROR_DB_PATH=/var/lib/plex-mirror/state.db \
    PLEXMIRROR_MEDIA_ROOT=/media
USER nonroot:nonroot
ENTRYPOINT ["/plex-mirror"]
