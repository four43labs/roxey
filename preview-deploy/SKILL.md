---
name: preview-deploy
description: Expose whatever you're currently running (a frontend dev server, a backend/API, a built app, a container) at a public https://<name>.f43.run URL using the roxey CLI. Use this whenever the user asks to "preview", "expose", or "show" a running app, or asks for a public URL to something you just started.
---

# Preview deploy

You expose running processes with `roxey` — a tunnel CLI that connects a
local port to a public subdomain through the relay at `relay.f43.run`. No
reverse-proxy config to hand-edit, no Docker networking gotchas: `roxey`
runs on the same host as your process and just needs `localhost:<port>`.

## Prerequisite (one-time per host)

`roxey` needs to be authenticated against the `f43.run` relay before it can
start tunnels. Check first:

```
roxey doctor
```

If it warns "no relay servers configured" (or fails on `f43.run`
specifically), run:

```
ROXEY_DOMAIN=f43.run roxey auth <api-key>
```

(use `npx -y @abhikrishnaram/roxey auth ...` if `roxey` isn't on `PATH`).
Ask the user for an API key from the relay dashboard if you don't have one.

## How to use it

1. Get whatever you want to preview running, bound to `localhost:<port>`
   (or `0.0.0.0:<port>` — either works, `roxey` connects locally).
2. Pick a subdomain name for it — **any name**, not restricted to
   "preview". Use something that describes what you're exposing, e.g.
   `myapp`, `myapp-api`, `checkout-fix`, `<branch-name>`:

   ```
   roxey start <name> localhost:<port>
   ```

   This prints the public URL immediately: `https://<name>.f43.run`.

3. If the thing you're previewing has a separate frontend and backend,
   start two tunnels with two related names, e.g.:

   ```
   roxey start myapp localhost:3000       # -> https://myapp.f43.run
   roxey start myapp-api localhost:8000   # -> https://myapp-api.f43.run
   ```

4. Tell the user the URL(s) that are live.
5. When done (or when reusing a name for something new):

   ```
   roxey stop <name>
   ```

   `roxey list` shows every tunnel currently running on this host.

## Notes

- A name is only usable by one tunnel at a time. If `roxey start` says
  "tunnel already running for `<name>` (pid ...)", either `roxey stop` the
  old one first or pick a different name — don't fight over shared names
  like the old fixed `preview`/`preview-api` scheme did.
- Traffic passes through Cloudflare and the relay, so `X-Forwarded-For`
  carries the real visitor IP — the connection's own remote address won't.
- `roxey` binaries/config are per-host (`~/.roxey/`); if you're previewing
  something on a different machine than usual, redo the one-time auth step
  there first.
- If a tunnel won't start or a URL isn't reachable, `roxey doctor` checks
  state, relay connectivity, certs, and port conflicts in one pass.
- For a project with a `roxey.yaml` manifest (multiple routes/services
  meant to come up together), prefer `roxey up [-d]` / `roxey down` over
  one-off `start` calls — `up -d` also spawns the manifest's own services,
  and `roxey logs <name>` tails one of them. Ad-hoc `start`/`stop` is for
  exposing something that isn't already described in a manifest.
