#!/bin/bash
# E2E smoke: relay + CLI `start`, manifest `up`/`down` (proxy + run routes).
# Runs entirely on high ports with ROXEY_RELAY_ADDR so no sudo/DNS needed.
# Sandboxes HOME so it never touches real ~/.roxey state.
set -e
cd "$(dirname "$0")/.."
BACKEND="$(pwd)/backend"; CLI_DIR="$(pwd)/cli"

RELAY=/tmp/smoke-roxey-relay
CLI=/tmp/smoke-roxey
( cd "$BACKEND" && go build -o "$RELAY" . )
( cd "$CLI_DIR"  && go build -o "$CLI" . )

WORK=$(mktemp -d)
FAKEHOME="$WORK/home"; mkdir -p "$FAKEHOME/.roxey"
export HOME="$FAKEHOME"

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ok: $1"; }
fail() { FAIL=$((FAIL+1)); echo "FAIL: $1"; }
check() { if eval "$2"; then ok "$1"; else fail "$1"; fi }

cleanup() {
  jobs -p | xargs kill 2>/dev/null || true
  pkill -f smoke-roxey-relay 2>/dev/null || true
  pkill -f "http.server 199" 2>/dev/null || true
  pkill -f "http.server 20001" 2>/dev/null || true
}
trap cleanup EXIT

echo "== starting relay (single-user local mode) =="
ROXEY_DOMAIN=smoke.localhost ROXEY_ADMIN_HOST=roxey.smoke.localhost \
ROXEY_SINGLE_USER=1 ROXEY_ADMIN_EMAIL=admin@localhost ROXEY_ADMIN_PASSWORD=pass PORT=18080 \
ROXEY_DB_PATH="$WORK/relay.db" "$RELAY" >"$WORK/relay.log" 2>&1 &

for i in $(seq 1 20); do
  curl -sf http://127.0.0.1:18080/healthz >/dev/null 2>&1 && break
  sleep 0.25
done
check "healthz" "curl -sf http://127.0.0.1:18080/healthz | grep -q ok"

# The seeded account logs in like any dashboard user; keys are created
# against its session.
admin_req() { curl -sf -H "Host: roxey.smoke.localhost" -b "$WORK/jar.txt" "$@"; }
check "login works" "curl -sf -c '$WORK/jar.txt' -H 'Host: roxey.smoke.localhost' \
  -H 'Content-Type: application/json' -d '{\"email\":\"admin@localhost\",\"password\":\"pass\"}' \
  http://127.0.0.1:18080/api/auth/login | grep -q admin@localhost"
check "signup disabled locally" "! curl -sf -H 'Host: roxey.smoke.localhost' \
  -H 'Content-Type: application/json' -d '{\"email\":\"x@y.z\",\"password\":\"longenough\"}' \
  http://127.0.0.1:18080/api/auth/signup >/dev/null"
KEY=$(admin_req -X POST http://127.0.0.1:18080/api/keys \
  -H 'Content-Type: application/json' -d '{"label":"smoke"}' | sed -n 's/.*"apiKey":"\([^"]*\)".*/\1/p')
check "api key created" "[ -n \"$KEY\" ]"

echo "== targets =="
python3 -m http.server 19999 --bind 127.0.0.1 >"$WORK/t2.log" 2>&1 &
sleep 0.4

echo "== roxey start (ad-hoc tunnel) =="
export ROXEY_DOMAIN=smoke.localhost
export ROXEY_RELAY_HOST=roxey.smoke.localhost
export ROXEY_RELAY_ADDR=127.0.0.1:18080
export ROXEY_INSECURE=1
printf '{"servers": {"smoke.localhost": {"apiKey": "%s"}}}' "$KEY" > "$FAKEHOME/.roxey/config.json"

"$CLI" start shop localhost:19999
sleep 0.7
check "config preserved after start" "grep -q \"$KEY\" \"$FAKEHOME/.roxey/config.json\""
check "tunnel serves target" "curl -sf -H 'Host: shop.smoke.localhost' http://127.0.0.1:18080/ | grep -q 'Directory listing'"
"$CLI" stop shop >/dev/null
sleep 0.5
check "stopped returns 502" "curl -s -o /dev/null -w '%{http_code}' -H 'Host: shop.smoke.localhost' http://127.0.0.1:18080/ | grep -q 502"

echo "== roxey up (manifest) =="
cat > "$WORK/roxey.yaml" <<EOF
relay_server:
  tld: smoke.localhost
environments:
  - host: app
    routes:
      "/": localhost:19999
      "/spawned":
        command: python3 -m http.server 20001 --directory $WORK/appdir
        port: 20001
EOF
mkdir -p "$WORK/appdir"
echo "spawned-service-content" > "$WORK/appdir/spawned"

"$CLI" up -d "$WORK/roxey.yaml"
sleep 1
check "root route proxied"     "curl -sf -H 'Host: app.smoke.localhost' http://127.0.0.1:18080/ | grep -q 'Directory listing'"
check "run-route spawned+up"   "curl -sf -H 'Host: app.smoke.localhost' http://127.0.0.1:18080/spawned | grep -q 'spawned-service-content'"
check "service recorded"       "\"$CLI\" list | grep -q '\[service\]'"

echo "== re-up restarts cleanly =="
OUT=$("$CLI" up -d "$WORK/roxey.yaml" 2>&1)
check "re-up restarts services" "echo \"\$OUT\" | grep -q 'restarted'"
sleep 1
check "routes live after re-up"  "curl -sf -H 'Host: app.smoke.localhost' http://127.0.0.1:18080/spawned | grep -q 'spawned-service-content'"

echo "== relay-side conflict surfaced =="
OUT=$("$CLI" _run smoke.localhost app "" localhost:19999 2>&1 || true)
check "409 reason shown"       "echo \"\$OUT\" | grep -q 'already in use'"

echo "== roxey down =="
"$CLI" down "$WORK/roxey.yaml" >/dev/null
sleep 0.7
check "tunnels torn down" "! curl -sf -H 'Host: app.smoke.localhost' http://127.0.0.1:18080/ >/dev/null"
check "spawned service killed" "! lsof -i :20001 -sTCP:LISTEN >/dev/null 2>&1"
check "state cleaned" "! \"$CLI\" list | grep -q smoke.localhost"

echo "== multi-project: registry + shared relay =="
mkdir -p "$WORK/projA" "$WORK/projB"
cat > "$WORK/projA/roxey.yaml" <<EOF
relay_server: {tld: smoke.localhost}
environments:
  - host: app.proja
    routes:
      "/": localhost:19999
EOF
cat > "$WORK/projB/roxey.yaml" <<EOF
relay_server: {tld: smoke.localhost}
environments:
  - host: app.projb
    routes:
      "/spawned":
        command: python3 -m http.server 20002 --directory $WORK/appdir
        port: 20002
EOF

"$CLI" up -d "$WORK/projA/roxey.yaml" >/dev/null
"$CLI" up -d "$WORK/projB/roxey.yaml" >/dev/null
sleep 1
check "project A live"   "curl -sf -H 'Host: app.proja.smoke.localhost' http://127.0.0.1:18080/ | grep -q 'Directory listing'"
check "project B live"   "curl -sf -H 'Host: app.projb.smoke.localhost' http://127.0.0.1:18080/spawned | grep -q 'spawned-service-content'"
check "registry has both" "\"$CLI\" projects | grep -c proj | grep -q 2"
"$CLI" down --project projA >/dev/null
sleep 0.7
check "down --project kills A only" "! curl -sf -H 'Host: app.proja.smoke.localhost' http://127.0.0.1:18080/ >/dev/null"
check "B survives A's down" "curl -sf -H 'Host: app.projb.smoke.localhost' http://127.0.0.1:18080/spawned | grep -q 'spawned-service-content'"
"$CLI" down --project projB >/dev/null
sleep 0.5
check "registry emptied by downs" "! \"$CLI\" projects | grep -qE '^proj'"

echo "== multi-user relay: signup, namespacing, protection =="
MU=18082
ROXEY_DOMAIN=mu.localhost ROXEY_ADMIN_HOST=roxey.mu.localhost PORT=$MU \
ROXEY_DB_PATH="$WORK/relay-mu.db" "$RELAY" >"$WORK/relay-mu.log" 2>&1 &
for i in $(seq 1 20); do
  curl -sf http://127.0.0.1:$MU/healthz >/dev/null 2>&1 && break
  sleep 0.25
done

mu_req() { curl -sf -H "Host: roxey.mu.localhost" "$@"; }
JAR_A="$WORK/jar-a.txt"; JAR_B="$WORK/jar-b.txt"
check "open signup works" "mu_req -c '$JAR_A' -H 'Content-Type: application/json' \
  -d '{\"email\":\"alice@x.test\",\"password\":\"password11\"}' http://127.0.0.1:$MU/api/auth/signup | grep -q alice@x.test"
check "duplicate email rejected" "! mu_req -H 'Content-Type: application/json' \
  -d '{\"email\":\"alice@x.test\",\"password\":\"password22\"}' http://127.0.0.1:$MU/api/auth/signup >/dev/null"

KEY_A=$(curl -sf -b "$JAR_A" -H "Host: roxey.mu.localhost" -H 'Content-Type: application/json' \
  -d '{"label":"laptop"}' http://127.0.0.1:$MU/api/keys | sed -n 's/.*"apiKey":"\([^"]*\)".*/\1/p')
check "key via session" "[ -n \"$KEY_A\" ]"

# Hosts are namespaced per account+service; the lookup endpoint is the
# client-visible mapping.
HOST_A=$(mu_req -H "Authorization: Bearer $KEY_A" \
  "http://127.0.0.1:$MU/api/lookup?services=pub" | sed -n 's/.*"pub":"\([^"]*\)".*/\1/p')
check "host namespaced" "echo \"$HOST_A\" | grep -q '^pub-.\{6\}$'"

printf '{"servers": {"mu.localhost": {"apiKey": "%s"}}}' "$KEY_A" > "$FAKEHOME/.roxey/config.json"
export ROXEY_DOMAIN=mu.localhost ROXEY_RELAY_HOST=roxey.mu.localhost ROXEY_RELAY_ADDR=127.0.0.1:$MU
"$CLI" start pub localhost:19999 >/dev/null
sleep 0.7
check "namespaced tunnel serves" \
  "curl -sf -H 'Host: $HOST_A.mu.localhost' http://127.0.0.1:$MU/ | grep -q 'Directory listing'"
check "bare host rejected" \
  "! curl -sf -H 'Host: pub.mu.localhost' http://127.0.0.1:$MU/ >/dev/null"

# Second account cannot see or revoke the first one's key.
check "second signup" "mu_req -c '$JAR_B' -H 'Content-Type: application/json' \
  -d '{\"email\":\"bob@x.test\",\"password\":\"password22\"}' http://127.0.0.1:$MU/api/auth/signup | grep -q bob@x.test"
check "keys scoped per user" \
  "! curl -sf -b '$JAR_B' -H 'Host: roxey.mu.localhost' http://127.0.0.1:$MU/api/keys | grep -q laptop"

"$CLI" stop pub >/dev/null

echo "== protected preview gate =="
"$CLI" start vault localhost:19999 --protect swordfish >/dev/null
sleep 0.7
HOST_V=$(mu_req -H "Authorization: Bearer $KEY_A" \
  "http://127.0.0.1:$MU/api/lookup?services=vault" | sed -n 's/.*"vault":"\([^"]*\)".*/\1/p')
VHOST_FQDN="$HOST_V.mu.localhost"
# Cookies key off the URL hostname, so gate-cookie checks go through
# --resolve instead of a Host header override.
vreq() { curl -s --resolve "$VHOST_FQDN:$MU:127.0.0.1" "$@"; }
check "gate blocks anonymous" \
  "curl -s -o /dev/null -w '%{http_code}' -H 'Host: $VHOST_FQDN' http://127.0.0.1:$MU/ | grep -q 401"
check "gate page renders" \
  "curl -s -H 'Host: $VHOST_FQDN' http://127.0.0.1:$MU/ | grep -q LOCKED"
check "wrong token blocked" \
  "! curl -sf -H 'Host: $VHOST_FQDN' 'http://127.0.0.1:$MU/?access_token=nope' >/dev/null"
check "token unlocks" \
  "curl -sf -H 'Host: $VHOST_FQDN' 'http://127.0.0.1:$MU/?access_token=swordfish' | grep -q 'Directory listing'"
GATE_JAR="$WORK/jar-gate.txt"
check "form unlock sets cookie" "vreq -sf -c '$GATE_JAR' -o /dev/null -d 'password=swordfish' 'http://$VHOST_FQDN:$MU/'"
check "cookie unlocks" \
  "vreq -sf -b '$GATE_JAR' 'http://$VHOST_FQDN:$MU/' | grep -q 'Directory listing'"
"$CLI" stop vault >/dev/null
unset ROXEY_DOMAIN ROXEY_RELAY_HOST ROXEY_RELAY_ADDR

echo
echo "passed=$PASS failed=$FAIL  (workdir $WORK)"
[ "$FAIL" = 0 ]
