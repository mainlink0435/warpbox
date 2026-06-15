# Warpbox — Multi-stage Docker build
#
# Stage 1: Build the Go binary with CGO (required by mattn/go-sqlite3).
FROM golang:1.26-alpine AS build

RUN apk add gcc musl-dev sqlite-dev

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN VERSION=$(git describe --tags --always 2>/dev/null || echo dev) && \
    CGO_ENABLED=1 go build -tags netgo \
    -ldflags="-s -w -extldflags=-static -X main.Version=${VERSION}" \
    -o /warpbox ./cmd/warpbox/

# ---------------------------------------------------------------------------
# Stage 2: Minimal runtime image.
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata wget

COPY --from=build /warpbox /usr/local/bin/warpbox

COPY docker-entrypoint.sh /usr/local/bin/
RUN chmod 755 /usr/local/bin/docker-entrypoint.sh

VOLUME /data
EXPOSE 1412

ENTRYPOINT ["docker-entrypoint.sh"]
