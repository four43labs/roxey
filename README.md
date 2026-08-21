# Roxey

ngrok-style tunnel for exposing local dev ports (or ports on a VPS) at
`https://<service>.<your-domain>`. Two independent Go modules:

- `backend/` — the relay: terminates `*.<domain>`, proxies HTTP requests to
  the connected CLI over a websocket, and serves a Basic-Auth REST API +
  dashboard on the admin host for API keys and live tunnels.
- `cli/` — the `roxey` binary users install locally or on a VPS.

## How it connects

1. CLI runs `roxey start <service> <local target>`. It spawns a detached
   background worker that opens one websocket to the relay
   (`wss://relay.<domain>/_ws`), authenticated with a bearer API key.
2. The relay registers that connection under `<service>` (and, for
   `service/path/*` specs, under a path prefix within that service).
3. A public request to `https://<service>.<domain>/...` is buffered and
   sent as a JSON frame over the matching websocket; the CLI worker replays
   it against the local target and ships the response back the same way.
4. `roxey stop` kills the local worker process; the relay notices the
   socket close and deregisters automatically.

Request/response bodies are base64'd JSON frames — fine for normal dev
traffic, not built for large file streaming or passing through the tunneled
app's own websocket connections (e.g. Vite/webpack HMR). Both would need a
binary framing upgrade.

## Deploying the relay

Point `*.roxey.run` (or your domain) at the relay's host through Cloudflare
(wildcard SSL, proxied). The relay itself speaks plain HTTP — Cloudflare
terminates TLS in front of it.

```
cd backend
cp .env.example .env   # set ROXEY_DOMAIN, ROXEY_ADMIN_USER/PASS
go run . -race=false   # or: go build -o roxey-relay . && ./roxey-relay
```

Visit `https://relay.<domain>/` for the dashboard (Basic Auth), generate an
API key there.

## Installing the CLI

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
