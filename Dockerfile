# Stage 1: Build frontend
FROM node:20-alpine AS frontend-builder

WORKDIR /app/web
COPY web/package*.json ./
RUN npm ci

COPY web/ ./
RUN npm run build

# Stage 2: Build Go backend
FROM golang:1.23-alpine AS backend-builder

RUN apk add --no-cache git gcc musl-dev

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=frontend-builder /app/web/dist ./web/dist

# CGO stays on. gorm's sqlite driver is mattn/go-sqlite3, which is cgo
# code: with CGO_ENABLED=0 the dependency builds a stub whose Open() returns
# "Binary was compiled with 'CGO_ENABLED=0'" — so the image started, failed
# to open the default database, and exited. The runtime is alpine, so the
# dynamic musl link the builder produces resolves there.
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-s -w" -o /app/yuarcloud ./cmd/server

# Stage 3: Runtime
FROM alpine:3.19

# su-exec drops privileges without a PAM/Tty layer, which is the whole job here:
# fix the mounted volumes, then become uid 1000 before the server ever starts.
RUN apk add --no-cache ca-certificates tzdata curl sqlite su-exec \
    && adduser -D -u 1000 yuarcloud

WORKDIR /app

COPY --from=backend-builder /app/yuarcloud /app/yuarcloud
COPY --from=backend-builder /app/configs/config.yaml.example /app/configs/config.yaml.example
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

# The container's own config, with absolute /srv and /config paths. See the
# header of the file for why relative ones are unsafe here.
COPY --from=backend-builder /app/configs/config.docker.yaml /config/config.yaml
# A second copy outside /config. The line above lands in the image layer, and
# a bind mount on /config hides that layer completely — which is exactly what
# the ZimaOS compose file does, leaving the server with no config at all.
# The entrypoint restores this one into an empty /config.
COPY --from=backend-builder /app/configs/config.docker.yaml /app/config.docker.yaml

# /srv holds user files, /config holds config.yaml and the SQLite database.
# Both are volumes; the binary only ever writes inside them. Chunked-upload
# scratch is not a volume of its own — it lives under /srv where the prefix keeps
# it out of the browser.
RUN mkdir -p /srv /srv/.yuarcloud-chunks /config \
    && chown -R yuarcloud:yuarcloud /srv /config /app \
    && chmod +x /usr/local/bin/docker-entrypoint.sh

# Starts as root so the entrypoint can hand the volumes to uid 1000 and drop to
# it. The server itself never runs as root; nothing that handles a request ever
# does. See docker-entrypoint.sh for why the alternative — telling everyone to
# chown the host directory first — is worse.
USER root

EXPOSE 3103
VOLUME ["/srv", "/config"]

# 3103, matching the port in config.docker.yaml. These were left on 8080 when
# the default moved, which is not cosmetic: the healthcheck probes the port the
# server is not listening on, so the container reports itself unhealthy forever
# while serving perfectly well, and anything gating on health — an orchestrator,
# a reverse proxy, a compose `depends_on: service_healthy` — waits for a
# condition that can never arrive.
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD curl -f http://localhost:3103/health || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["/app/yuarcloud", "/config/config.yaml"]