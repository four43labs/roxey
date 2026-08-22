# Roxey

ngrok-style tunnel for exposing local dev ports (or ports on a VPS) at
`https://<service>.<your-domain>`. Two independent Go modules:

- `backend/` — the relay: terminates `*.<domain>`, pipes each connection as a
  raw TCP stream to the connected CLI over a multiplexed websocket, and serves
  a Basic-Auth REST API + dashboard on the admin host for API keys and live
  tunnels.
- `cli/` — the `roxey` binary users install locally or on a VPS.

## How it connects

1. CLI runs `roxey start <service> <local target>`. It spawns a detached
   background worker that opens one websocket to the relay
   (`wss://relay.<domain>/_ws`), authenticated with a bearer API key.
2. The relay registers that connection under `<service>` (and, for
   `service/path/*` specs, under a path prefix within that service).
3. A public request to `https://<service>.<domain>/...` is streamed over a
   fresh raw TCP stream (yamux-multiplexed inside the CLI's websocket); the
   CLI dials the local target's port and pipes bytes both ways until either
   side closes.
4. `roxey stop` kills the local worker process; the relay notices the
   socket close and deregisters automatically.

Because each connection is a raw byte pipe, anything on top of HTTP passes
through untouched: websocket upgrades (Vite/webpack HMR), SSE/chunked
responses, and large uploads/downloads all stream without buffering.

## Deploying the relay

Point `*.roxey.run` (or your domain) at the relay's host through Cloudflare
(wildcard SSL, proxied). The relay itself speaks plain HTTP — Cloudflare
terminates TLS in front of it.

Build and run directly:

```
cd backend
cp .env.example .env   # set ROXEY_DOMAIN, ROXEY_ADMIN_USER/PASS
go build -o roxey-relay . && ./roxey-relay
```

Or via the prebuilt image, published to GHCR on every push to `main`
(`.github/workflows/docker.yml`) and on version tags:

```
docker run -d -p 8080:8080 \
  -e ROXEY_DOMAIN=roxey.run \
  -e ROXEY_ADMIN_USER=admin -e ROXEY_ADMIN_PASS=change-me \
  -v roxey-data:/data \
  ghcr.io/four43labs/roxey-relay:latest
```

The image writes its SQLite file to `/data/roxey.db` (`ROXEY_DB_PATH`,
already set in the image) — mount `/data` as a volume so API keys and
tunnel history survive container restarts.

Visit `https://relay.<domain>/` for the dashboard (Basic Auth), generate an
API key there.

## Installing the CLI

No install, via npx (macOS/Linux) — `npm/bin/roxey.js` downloads the
matching binary from the GitHub Release on first run and caches it in
`~/.roxey/bin`:

```
npx @abhikrishnaram/roxey auth <api-key-from-dashboard>
npx @abhikrishnaram/roxey start myapp localhost:3000
```

Or download a prebuilt binary from the [releases page](https://github.com/four43labs/roxey/releases)
(published by `.github/workflows/release.yml` on `v*.*.*` tags — linux/darwin,
amd64/arm64), or build from source:

```
cd cli
go build -o roxey .
mv roxey /usr/local/bin/

roxey auth <api-key-from-dashboard>          # ROXEY_DOMAIN env picks the domain, defaults roxey.run
roxey start myapp localhost:3000             # -> https://myapp.roxey.run
roxey start "myapp/api/*" localhost:8000     # -> https://myapp.roxey.run/api/*
roxey list
roxey stop myapp
```

Set `ROXEY_DOMAIN` / `ROXEY_RELAY_HOST` at `auth` time to point at a
self-hosted relay; `ROXEY_INSECURE=1` on `start` skips TLS for testing a
relay over plain `ws://`.

## Cutting a release

Tag `vX.Y.Z` and push it — this triggers both workflows: the relay image
is built and pushed to `ghcr.io/four43labs/roxey-relay:X.Y.Z` (and
`:latest`), and the CLI release workflow cross-builds the CLI, publishes it
to a GitHub Release, then publishes `npm/` to the npm registry as
`@abhikrishnaram/roxey@X.Y.Z`
(needs an `NPM_TOKEN` repo secret with publish access).

```
git tag v0.1.0
git push origin v0.1.0
```
