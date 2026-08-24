Roxey is a tunneling and local reverse-proxy tool for development teams. Point it at the services running on your machine — or let it start them for you — and they become reachable at clean, HTTPS URLs like `https://myapp.f43.run` or `https://api.localhost`, with path-based routing between multiple services on a single domain.

It is designed for the messy reality of modern local development: a frontend, a backend, a worker, maybe a second site — all needing to share one origin, one set of OAuth callbacks, and one URL scheme that matches production.

## Features

- **Named HTTPS URLs** — expose any local port as `https://<name>.<domain>`, no ports to remember
- **Path-based routing** — split one domain across multiple services (`/api/*` → backend, `/` → frontend), like an nginx ingress but without the config ceremony
- **Compose-style manifests** — describe your whole project topology in a single `roxey.yaml` with `roxey up` / `roxey down`
- **Service spawning** — roxey can run your dev commands itself (portless-style), inject `PORT`/`HOST`/custom env vars, wait for readiness, and clean up on Ctrl-C
- **Local relay mode** — run the relay backend on your own machine for instant `.localhost` HTTPS domains, complete with automatic CA generation and trust-store installation
- **Everything streams** — tunnels are raw TCP pipes, so WebSockets, SSE, HMR, and large uploads just work
- **Self-hostable** — use the hosted relay at `relay.f43.run`, or deploy your own with a single Docker image

## Getting Started

### Installation

No install step via npx (macOS/Linux) — the binary is downloaded from GitHub Releases on first run and cached in `~/.roxey/bin`:

```bash
npx @four43labs/roxey --help
```

Or install globally:

```bash
npm install -g @four43labs/roxey
```

Or grab a prebuilt binary directly from the [releases page](https://github.com/four43labs/roxey/releases), or build from source:

```bash
git clone https://github.com/four43labs/roxey && cd roxey
cd cli && go build -o roxey . && mv roxey /usr/local/bin/
```

### Authenticating

Create an API key on the [relay dashboard](https://relay.f43.run) (Basic Auth), then:

```bash
roxey auth <api-key-from-dashboard>
# or paste interactively:
roxey auth
```

Keys are stored per relay in `~/.roxey/config.json`. Working against multiple relays? Save one per TLD:

```bash
roxey auth --tld dev.mycompany.dev <key>
```

### Quick tunnel: expose one port

The fastest way to try Roxey — put a local dev server on a public URL:

```bash
roxey start myapp localhost:3000
# -> https://myapp.f43.run
```

### Path routing: many services, one domain

Split a single hostname across services using `<service>/<path>/*` specs. The full request path is forwarded unchanged, so backends see exactly what production would send them:

```bash
roxey start myapp localhost:3000            # root of the subdomain
roxey start "myapp/api/*" localhost:8000    # /api/* -> backend
roxey list                                  # see what's running
roxey stop myapp                            # stops all routes for myapp
```

### Project topology: `roxey.yaml` and `up`/`down`

For multi-service projects, declare everything in a manifest instead of running ad-hoc commands. Routes either proxy to a running address or let roxey spawn the service itself:

```yaml
relay_server:
  tld: f43.run              # remote relay at relay.f43.run
  # api_key: rxy_...        # optional; falls back to saved auth state
  # local: true             # or run the relay locally (see below)

environments:
  - host: shop              # -> https://shop.f43.run
    routes:
      "/": website:3001     # proxy to a running address...
      "/api": localhost:8000
      "/storefront":        # ...or have roxey spawn the command itself
        command: npm run dev
        cwd: apps/web       # optional; default is the manifest's directory
        port: 3000          # required for spawned services (exported as PORT)
        environment:        # extra env vars for the child process
          NODE_ENV: development
```

```bash
roxey up      # foreground: interleaved [name] logs, Ctrl-C tears down everything
roxey up -d   # detached: prints a URL table, logs via `roxey logs <name>`
roxey down    # stop this manifest's tunnels and spawned services
```

Spawned services receive `PORT`, `HOST=127.0.0.1`, and their `environment`
map. `up` waits until each port accepts connections before opening its
tunnel, and re-running it restarts the manifest's services and tunnels from
a clean slate — a crashed run can never leave orphans behind.

### Local development domains: `local` relay mode

Skip the hosted relay entirely and run one on your own machine — portless-style local domains with real HTTPS:

```yaml
relay_server:
  local: true           # tld defaults to "localhost"...
  # tld: dev.mycompany.dev   # ...but ANY domain works, not just .localhost
```

On first `up`, roxey downloads the relay binary, generates a local CA, asks once for sudo to trust it and bind port 443, creates its own API key, and starts the relay. You then browse `https://shop.localhost` — no cert warnings, no manual setup.

**Any TLD works locally, not just `.localhost`.** With `tld: dev.mycompany.dev` you get `https://shop.dev.mycompany.dev` — URLs shaped exactly like production. Roxey syncs the needed names into `/etc/hosts` under a managed block (removed by `roxey down`) and covers them all with one wildcard-style certificate.

#### When to use which

- **`local: true` with `.localhost` (default)** — solo work and quick iteration. `.localhost` names resolve natively in Chrome/Firefox/Edge; zero DNS or privileges beyond a one-time sudo prompt.
- **`local: true` with a custom TLD** — when you need production-shaped URLs: strict OAuth providers (Google, Apple) reject `.localhost` redirect URIs but accept real domains, and cross-subdomain cookies (`shop` ↔ `api` on one TLD) behave exactly as they will in prod.
- **Hosted / self-hosted remote relay** — when teammates, devices on your LAN, or external webhooks must reach your dev environment.

#### Tips for local dev

- OAuth callbacks work as-is under a custom TLD: register `https://app.dev.mycompany.dev/api/auth/callback/google` once and it works across every branch.
- Give each service its own subdomain (`api.*`, `admin.*`) rather than path prefixes when apps hard-code origins or cookies — everything still runs on your machine.
- The relay keeps running after `roxey down` so subsequent `up`s are instant; stop it manually with `pkill roxey-relay`.
- Everything binds to loopback only — nothing is reachable from other machines unless you also point a remote relay at them.

## Commands

```
roxey auth [--tld <tld>] [api-key]
  # Save an API key for a relay server (prompts if key omitted)

roxey up [-d] [file]
  # Bring up every environment in a roxey.yaml manifest
  #   -d, --detach   Run detached; print URL table instead of streaming logs
  #   file           Manifest path; defaults to ./roxey.yaml

roxey down [file]
  # Stop the tunnels and spawned services owned by a manifest

roxey logs <name>
  # Tail the log of a service started with `up -d`

roxey start <service>[/path/*] <target>
  # Start an ad-hoc tunnel, e.g. roxey start myapp localhost:3000

roxey stop <service>[/path]
  # Stop a running tunnel (bare service name stops all of its routes)

roxey list
  # List live tunnels and spawned services on this machine
```

Environment variables:

| Variable | Purpose |
|---|---|
| `ROXEY_DOMAIN` | Default TLD when no manifest/flag specifies one (default: `f43.run`) |
| `ROXEY_RELAY_HOST` | Override the relay hostname (`relay.<tld>` by default) |
| `ROXEY_INSECURE=1` | Connect tunnels over plain `ws://` (testing relays without TLS) |
| `ROXEY_RELAY_BIN` | Use a local relay binary instead of downloading one |

## How It Works

Roxey has two halves: a **relay** that answers public requests, and a **CLI** that connects your machine to it.

```mermaid
flowchart LR
    subgraph machine["your machine"]
        W["roxey worker"] --> FE[":3000 frontend"]
        W --> BE[":8000 backend"]
    end

    B["browser / curl"] -- "https://shop.f43.run/api/..." --> R["relay<br/>(relay.f43.run)"]
    R <-- "one yamux WebSocket,<br/>many raw TCP streams" --> W
```

1. `roxey start` (or `up`) spawns a background worker that opens a single WebSocket to the relay at `wss://relay.<tld>/_ws`, authenticated with your API key.
2. The relay registers that connection under the requested service name (and path prefix, if any).
3. A public request to `https://<service>.<tld>/...` is streamed over a fresh raw TCP stream, multiplexed through the CLI's WebSocket with yamux. The CLI dials the local target's port and pipes bytes in both directions until either side closes.
4. When the worker disconnects (`roxey stop`, Ctrl-C, network loss), the relay deregisters the route immediately.

Because each connection is an opaque byte pipe, anything spoken over HTTP passes through untouched — WebSocket upgrades (Vite/Next HMR), SSE, chunked uploads, and large downloads all stream without buffering. Routing decisions happen only at the edge: subdomain selects the environment, longest-matching registered path prefix selects the backend.

In `local` mode the same architecture runs entirely on your machine: the relay binds `127.0.0.1:443` behind a locally-generated wildcard certificate, so nothing leaves your network.

## Project Modules

The repository is two independent Go modules plus an npm wrapper:

- **`backend/`** — the relay. A single HTTP server that terminates traffic for `*.<domain>`, resolves each request against an in-memory routing table, and pipes connections as raw TCP streams over multiplexed WebSockets. Also serves a Basic-Auth admin API and dashboard for API keys and live tunnel status, backed by SQLite. Ships as a Docker image and prebuilt binaries.
- **`cli/`** — the `roxey` binary users install. Handles authentication, manifest loading/validation, spawning and supervising services, and running the background tunnel workers. This is what npm distributes as `@four43labs/roxey`.
- **`npm/`** — a thin installer package; downloads the matching platform binary from GitHub Releases on first run.

## Choosing the Relay Backend

By default the CLI talks to the relay operated by Four43 Labs at `relay.f43.run` — no setup beyond `roxey auth`. You have three options, selected per project in the manifest's `relay_server` block:

1. **Hosted relay** (default) — `tld: f43.run`; public HTTPS URLs out of the box.
2. **Local relay** (`local: true`) — the relay runs on your machine; see [Local development domains](#local-development-domains-local-relay-mode). Best for solo dev with production-shaped URLs and nothing exposed externally.
3. **Self-hosted remote relay** — your own deployment of the image below, for teams that need private infrastructure or a custom domain.

### Deploying Your Own Relay Backend

Point a wildcard DNS record (`*.your-domain.com`) at your server — through Cloudflare or any proxy that terminates TLS — then run the image:

```bash
docker run -d -p 8080:8080 \
  -e ROXEY_DOMAIN=your-domain.com \
  -e ROXEY_ADMIN_USER=admin \
  -e ROXEY_ADMIN_PASS=change-me \
  -v roxey-data:/data \
  ghcr.io/four43labs/roxey-relay:latest
```

The relay writes its SQLite database to `/data/roxey.db` (`ROXEY_DB_PATH`); mount the volume so API keys survive restarts. Then visit `https://relay.your-domain.com` to generate keys, and authenticate clients with `ROXEY_DOMAIN=your-domain.com roxey auth <key>`.

Additional server options:

| Variable | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | Listen port |
| `ROXEY_ADMIN_HOST` | `relay.<domain>` | Host serving the admin API/dashboard |
| `ROXEY_DB_PATH` | `roxey.db` | SQLite location |
| `ROXEY_TLS_CERT` / `ROXEY_TLS_KEY` | unset | Serve HTTPS directly (otherwise terminate TLS upstream) |

Releases are tagged `vX.Y.Z`; a single tag publishes the Docker image, both binaries, and the npm package together.

## Comparison

| | Roxey | ngrok | Portless | Custom nginx |
|---|---|---|---|---|
| Named HTTPS URLs | ✅ | ✅ | ✅ `.localhost` | Manual |
| Shareable public URLs | ✅ | ✅ | ❌ (local/LAN only) | If publicly hosted |
| Path routing on one domain | ✅ | Paid plans | ❌ | ✅ |
| Declarative multi-service manifests | ✅ | ❌ | Partial | Hand-written configs |
| Starts your dev commands | ✅ | ❌ | ✅ | ❌ |
| Local-only mode (no cloud) | ✅ | ❌ | ✅ | ✅ |
| Self-hosted relay | ✅ single image | Enterprise | n/a | n/a |
| Setup effort | One command | One command | One command | Significant |

## Requirements

- macOS or Linux (Windows builds are not published yet)
- Node.js ≥ 18 for the npm installer, or use a prebuilt binary
- Go ≥ 1.24 only if building from source
- For local relay mode: sudo access (first run only, to trust the CA and bind port 443)

## License

This project and library is MIT licensed. See [`LICENSE`](LICENSE) for details.
