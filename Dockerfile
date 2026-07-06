# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build
WORKDIR /src

# Version baked into the binary. Build each canary image with e.g.
#   docker build --build-arg VERSION=1.0.0 -t guestbook:1.0.0 .
#   docker build --build-arg VERSION=2.0.0 -t guestbook:2.0.0 .
ARG VERSION=dev

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

# Build a static binary (the web UI is embedded via go:embed, so no assets to copy).
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" -o /out/guestbook .

RUN mkdir -p /data

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/guestbook /app/guestbook

# Writable /data volume for the SQLite database, owned by the non-root user.
COPY --from=build --chown=nonroot:nonroot /data /data
VOLUME ["/data"]

ENV PORT=8080 \
    DB_PATH=/data/guestbook.db
EXPOSE 8080

# Runs as the non-root user provided by the distroless image.
ENTRYPOINT ["/app/guestbook"]
