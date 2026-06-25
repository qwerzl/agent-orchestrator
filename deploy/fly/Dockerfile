# AO daemon image for Fly.io. Build context is the repo root:
#   fly deploy --config deploy/fly/fly.toml
# Sessions run in Fly Sprites (AO_RUNTIME=flysprite), so the daemon image itself
# needs no zellij/git — just the static Go binary and CA certs for HTTPS to the
# Sprites API, the Anthropic API, and GitHub.

FROM golang:1.25-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
# Cache module downloads on the go.mod/go.sum layer.
COPY backend/go.mod backend/go.sum ./backend/
RUN cd backend && go mod download
COPY backend/ ./backend/
# modernc.org/sqlite is pure Go, so CGO can stay off for a static binary.
RUN cd backend && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/ao ./cmd/ao

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/ao /usr/local/bin/ao
# Defaults; secrets (AO_AUTH_TOKEN, AO_SPRITES_TOKEN, AO_CLAUDE_CREDENTIALS,
# AO_GITHUB_TOKEN, AO_PUBLIC_URL) are supplied via `fly secrets`.
ENV AO_PORT=8080 \
    AO_BIND_HOST=0.0.0.0 \
    AO_RUNTIME=flysprite \
    AO_DATA_DIR=/data
EXPOSE 8080
# Runs as root so the mounted Fly volume at /data is writable.
ENTRYPOINT ["ao", "daemon"]
