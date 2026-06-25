# Hosting the AO daemon on Railway

Same daemon, same Fly-Sprite sandboxes — just hosted on Railway instead of
Fly.io (handy if `fly deploy` is blocked on your network, e.g. `depot.fly.io`
unreachable). The daemon only needs outbound HTTPS to `api.sprites.dev`, which
Railway provides.

`railway.json` (repo root) points Railway's Dockerfile builder at
`deploy/fly/Dockerfile` (the same image used for Fly). The daemon binds Railway's
injected `$PORT` automatically.

## Prerequisites

- Railway CLI: `npm i -g @railway/cli` (or `brew install railway`), then `railway login`.
- The same credentials as the Fly path: a Fly **Sprites** token, a **GitHub** token,
  and your full-scope **claude.ai** credential (see `deploy/fly/README.md` →
  "Remote Control credential").

## 1. Create the project + service

```sh
# from the repo root
railway init                 # create a new project (or `railway link` to an existing one)
railway up                   # builds deploy/fly/Dockerfile and deploys
```

## 2. Set variables

The daemon refuses to bind a public host without an auth token, so set these
before (or right after) the first deploy. Railway injects `PORT` itself.

```sh
railway variables \
  --set "AO_AUTH_TOKEN=$(openssl rand -hex 32)" \
  --set "AO_SPRITES_TOKEN=<your-fly-sprites-token>" \
  --set "GITHUB_TOKEN=<your-github-token>" \
  --set "AO_CLAUDE_CREDENTIALS=$(cat claude-credentials.json)"
```

`GITHUB_TOKEN` serves both the daemon's PR-status observer and the sprite git auth
(the sprite token resolves `AO_GITHUB_TOKEN` → `GITHUB_TOKEN` → `GH_TOKEN`).

(`AO_BIND_HOST=0.0.0.0`, `AO_RUNTIME=flysprite`, and `AO_DATA_DIR=/data` come from
the image; `AO_PORT` is intentionally unset so the daemon binds Railway's `$PORT`.)

## 3. Public domain + AO_PUBLIC_URL

```sh
railway domain                                   # generates https://<name>.up.railway.app
railway variables --set "AO_PUBLIC_URL=https://<name>.up.railway.app"
railway up                                       # redeploy so sprites get AO_DAEMON_URL
```

## 4. Persistent volume (SQLite)

Add a volume mounted at **`/data`** (matches `AO_DATA_DIR`) via the Railway
dashboard (Service → Settings → Volumes → mount path `/data`), or:

```sh
railway volume add --mount-path /data
```

## 5. Verify + use

```sh
curl https://<name>.up.railway.app/healthz       # {"status":"ok",...}

# from your laptop:
export AO_DAEMON_URL="https://<name>.up.railway.app"
export AO_AUTH_TOKEN="<the token you set>"
ao project add <a repo with a GitHub origin>
ao spawn --project <id> --prompt "…"
ao attach <session>
```

## Notes

- Railway respects the repo-root `.dockerignore`, so the build context stays small.
- If `railway up` can't find the config, ensure you run it from the repo root (so it
  sees `railway.json`).
- Everything about the **sandboxes** is unchanged — they still run on Fly Sprites via
  `AO_SPRITES_TOKEN`.
