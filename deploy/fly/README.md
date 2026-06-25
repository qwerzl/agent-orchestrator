# Hosting the AO daemon on Fly.io

This deploys the AO daemon to Fly.io and runs each Claude Code session in a
**Fly Sprite**. You drive sessions from your laptop with `ao attach` (over the
authenticated public URL) and steer them from claude.ai via Remote Control.

```
 your laptop ──HTTPS/WSS + bearer token──▶  AO daemon (Fly Machine)
   ao attach / ao session / web UI                   │  Sprites API
                                                     ▼
                                       Fly Sprites (one per session)
                                       claude --remote-control in zellij
```

## Prerequisites

- `flyctl` installed and logged in (`fly auth login`).
- A **Fly Sprites API token** (from sprites.dev / `sprite org auth`).
- A **GitHub token** (clone/push + `gh` inside sprites).
- Your **claude.ai login credential** — see "Remote Control credential" below.

## 1. Create the app + volume

Fly app names are **globally unique** (not per-account), so pick your own and
keep `app` in `fly.toml`, the `--app` flags, and `AO_PUBLIC_URL` all in sync. The
volume name must match `[mounts] source` in `fly.toml` (`ao_data`).

```sh
# from the repo root
fly apps create tao-daemon          # use your own globally-unique name; match `app` in fly.toml
fly volumes create ao_data --size 1 --region iad --app tao-daemon
```

## 2. Set secrets

```sh
fly secrets set --app tao-daemon \
  AO_AUTH_TOKEN="$(openssl rand -hex 32)" \
  AO_SPRITES_TOKEN="<your-fly-sprites-token>" \
  AO_GITHUB_TOKEN="<your-github-token>" \
  AO_PUBLIC_URL="https://tao-daemon.fly.dev" \
  AO_CLAUDE_CREDENTIALS="$(cat claude-credentials.json)"
```

- **`AO_AUTH_TOKEN`** is the bearer token the daemon requires on every API call
  and the `/mux` WebSocket. Keep it; you give it to the CLI/web app below.
- **`AO_PUBLIC_URL`** must be the app's real URL — it is injected into each sprite
  as `AO_DAEMON_URL` so in-sprite `ao hooks` reach the daemon.

### Remote Control credential

Remote Control requires a **full-scope `claude auth login` token**, _not_ a
long-lived `claude setup-token` / `CLAUDE_CODE_OAUTH_TOKEN` (those are
inference-only and Remote Control is refused with them). On the machine where you
ran `claude auth login`, export the credential:

```sh
# macOS (credential lives in the Keychain):
security find-generic-password -s "Claude Code-credentials" -w > claude-credentials.json
# Linux (file-based):
cp ~/.claude/.credentials.json claude-credentials.json
```

Then pass it as `AO_CLAUDE_CREDENTIALS` (above). The daemon writes it to
`~/.claude/.credentials.json` in each sprite. **Note:** validate with one session
first — sharing a single login token across many concurrent sprites may trip
token-rotation / "logged in elsewhere" behavior.

## 3. Deploy

```sh
fly deploy --config deploy/fly/fly.toml
fly status --app tao-daemon
curl https://tao-daemon.fly.dev/healthz   # {"status":"ok",...}
```

## 4. Use it from your laptop

```sh
export AO_DAEMON_URL="https://tao-daemon.fly.dev"
export AO_AUTH_TOKEN="<the AO_AUTH_TOKEN you set above>"

ao project add <a repo with a GitHub origin>   # the sprite clones its origin URL
ao spawn --project <id> --prompt "…"           # runs in a sprite, Remote Control on
ao session ls
ao attach <session>                            # interactive terminal; Ctrl-] detaches
```

The web app authenticates by POSTing the token to `/api/v1/auth/login`, which
sets an `ao_token` cookie (so the EventSource stream + mux work).

## Notes / current limitations

- **Sprite tooling:** the runtime installs `zellij` into each sprite on first
  spawn (a few seconds). For faster spawns and working activity hooks, prebuild a
  sprite template with `zellij` **and** the `ao` binary preinstalled (see
  `sprite-template.sh`) — without `ao` in the sprite, `ao hooks` callbacks are a
  no-op and the daemon falls back to runtime-liveness for status.
- **Projects need a clonable origin:** a project with no GitHub remote URL cannot
  run in a sprite (the sprite clones over the network).
- **Web UI hosting:** this image serves the daemon API only. Serving the renderer
  as a hosted web app (vs the Electron desktop app) is a follow-up.
