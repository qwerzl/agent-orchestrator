#!/usr/bin/env bash
# Optional: prepare a Fly Sprite "template" with zellij + the ao binary
# preinstalled, so spawns are faster and in-sprite `ao hooks` callbacks work.
#
# The flysprite runtime currently creates each sprite from the base image and
# installs zellij on first spawn (a few seconds), and does NOT yet restore from a
# checkpoint. This script prepares a richer base + checkpoint for that future
# path; until the adapter restores from it, it serves to validate the tooling.
#
# Requires: the `sprite` CLI (sprites.dev) authenticated, plus Go on this machine.
# Usage: deploy/fly/sprite-template.sh [template-name]
set -euo pipefail

NAME="${1:-ao-template}"
ZELLIJ_VERSION="v0.44.3"
HERE="$(cd "$(dirname "$0")" && pwd)"

echo "==> building ao for linux/amd64"
( cd "$HERE/../../backend" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /tmp/ao-linux ./cmd/ao )

echo "==> creating sprite $NAME"
sprite create "$NAME"

echo "==> installing zellij"
sprite exec "$NAME" -- bash -lc "
  set -e
  mkdir -p \"\$HOME/.local/bin\"
  curl -fsSL https://github.com/zellij-org/zellij/releases/download/${ZELLIJ_VERSION}/zellij-x86_64-unknown-linux-musl.tar.gz -o /tmp/z.tgz
  tar xzf /tmp/z.tgz -C \"\$HOME/.local/bin\"
  zellij --version
"

echo "==> uploading ao binary"
# NOTE: confirm the file-copy flag for your `sprite` CLI version.
sprite exec "$NAME" --file /tmp/ao-linux:/home/sprite/.local/bin/ao
sprite exec "$NAME" -- bash -lc 'chmod +x "$HOME/.local/bin/ao" && ao version || true'

echo "==> checkpointing"
sprite checkpoint "$NAME"

echo "==> template '$NAME' ready (wire its checkpoint id into the flysprite"
echo "    adapter once restore-from-checkpoint is implemented)."
