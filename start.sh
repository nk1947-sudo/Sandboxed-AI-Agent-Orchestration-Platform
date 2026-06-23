#!/usr/bin/env bash
# start.sh — bring up the full Sandbox Hypervisor stack on WSL2 in one command.
#
#   Redis + Postgres + nginx UI (Docker) + control plane (host)
#
# Run as root from the repo root:  sudo ./start.sh
# Then open http://localhost:3000  (login: admin / your ADMIN_PASSWORD).
#
# Idempotent: safe to re-run; it restarts the control plane and ensures the
# containers/services are up. Reads all config from .env.

cd "$(dirname "$0")" || exit 1
REPO="$(pwd)"

log() { printf '[start] %s\n' "$*"; }

[ -f .env ] || { echo "[!] .env not found in $REPO — copy .env.example to .env first"; exit 1; }
[ -x ./controlplane ] || { echo "[!] ./controlplane not built — run: go build -o controlplane ./cmd/controlplane/"; exit 1; }

# ── host services ─────────────────────────────────────────────────────────────
log "starting docker + redis…"
service docker start        >/dev/null 2>&1 || true
service redis-server start  >/dev/null 2>&1 || true
redis-cli ping >/dev/null 2>&1 || { echo "[!] Redis not responding"; exit 1; }

# ── containers (Postgres + UI) ────────────────────────────────────────────────
if ! docker start sandbox-pg >/dev/null 2>&1; then
  echo "[!] sandbox-pg container missing. Create it once:"
  echo "    docker run -d --name sandbox-pg -e POSTGRES_USER=postgres \\"
  echo "      -e POSTGRES_PASSWORD=Admin@123 -e POSTGRES_DB=sandbox \\"
  echo "      -p 127.0.0.1:5432:5432 -v sandbox-pgdata:/var/lib/postgresql/data postgres:16-alpine"
  exit 1
fi
if ! docker start sandbox-ui >/dev/null 2>&1; then
  echo "[!] sandbox-ui container missing. Build it once:  docker build -t sandbox-ui ./web"
  echo "    then: docker run -d --name sandbox-ui --network host sandbox-ui"
fi

log "waiting for Postgres…"
until docker exec sandbox-pg pg_isready -U postgres -d sandbox >/dev/null 2>&1; do sleep 1; done

# ── control plane (host) ──────────────────────────────────────────────────────
log "restarting control plane…"
pkill -f '/controlplane$' 2>/dev/null || true
sleep 1
set -a; . ./.env; set +a
nohup ./controlplane > /tmp/cp.log 2>&1 &
sleep 3

log "control-plane startup:"
grep -E 'postgres data layer|server listening|"level":"ERROR"' /tmp/cp.log | tail -5 || true

echo
echo "[ok] stack up:"
echo "       UI   → http://localhost:3000   (login: admin / \$ADMIN_PASSWORD)"
echo "       API  → http://localhost:7777"
echo "       logs → tail -f /tmp/cp.log"
