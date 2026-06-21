#!/usr/bin/env bash
# host-jailer-setup.sh — Phase 6: create the unprivileged jaileruser account,
# the jailer chroot tree, and the sandbox state directory.
#
# Run as root on the KVM host BEFORE starting the control plane.
# Safe to re-run; all operations are idempotent.
set -euo pipefail

JAILER_UID=10001
JAILER_GID=10001
JAILER_USER=jaileruser
JAILER_GROUP=jaileruser
CHROOT_BASE=/srv/jailer
STATE_DIR=/srv/sandbox-state
FC_BIN=${FC_BIN:-/usr/local/bin/firecracker}
JAILER_BIN=${JAILER_BIN:-/usr/local/bin/jailer}

log() { echo "[jailer-setup] $*"; }

# ── group / user ────────────────────────────────────────────────────────────

if ! getent group "$JAILER_GROUP" >/dev/null 2>&1; then
  log "Creating group $JAILER_GROUP (gid $JAILER_GID)"
  groupadd --system --gid "$JAILER_GID" "$JAILER_GROUP"
else
  log "Group $JAILER_GROUP already exists"
fi

if ! id "$JAILER_USER" >/dev/null 2>&1; then
  log "Creating user $JAILER_USER (uid $JAILER_UID)"
  useradd \
    --system \
    --uid "$JAILER_UID" \
    --gid "$JAILER_GID" \
    --no-create-home \
    --shell /sbin/nologin \
    --comment "Firecracker jailer — no login" \
    "$JAILER_USER"
else
  log "User $JAILER_USER already exists"
fi

# ── directories ─────────────────────────────────────────────────────────────

log "Creating $CHROOT_BASE"
mkdir -p "$CHROOT_BASE"
# The jailer owns the chroot base so it can create/remove per-VM subtrees.
chown "$JAILER_USER:$JAILER_GROUP" "$CHROOT_BASE"
chmod 0750 "$CHROOT_BASE"

log "Creating $STATE_DIR"
mkdir -p "$STATE_DIR"
# State dir is owned by root; the control-plane (root) writes here,
# the guest-agent data is isolated inside per-VM overlays.
chmod 0700 "$STATE_DIR"

# ── binary permissions ───────────────────────────────────────────────────────
# The jailer binary runs setuid-root so it can drop privileges from root to
# jaileruser. Firecracker itself must be executable by jaileruser.

if [[ -f "$JAILER_BIN" ]]; then
  log "Setting jailer binary permissions ($JAILER_BIN)"
  chown root:root "$JAILER_BIN"
  chmod 4755 "$JAILER_BIN"   # setuid root
else
  log "WARNING: $JAILER_BIN not found — install Firecracker before starting"
fi

if [[ -f "$FC_BIN" ]]; then
  log "Setting firecracker binary permissions ($FC_BIN)"
  chown root:"$JAILER_GROUP" "$FC_BIN"
  chmod 0750 "$FC_BIN"
else
  log "WARNING: $FC_BIN not found — install Firecracker before starting"
fi

# ── /dev/kvm ACL ─────────────────────────────────────────────────────────────
# jaileruser needs read-write on /dev/kvm to create KVM VMs.

if [[ -c /dev/kvm ]]; then
  log "Granting $JAILER_USER access to /dev/kvm via kvm group"
  # Most distros have a 'kvm' group; add jaileruser to it.
  if getent group kvm >/dev/null 2>&1; then
    usermod -aG kvm "$JAILER_USER"
  else
    log "No kvm group found; using setfacl"
    if command -v setfacl >/dev/null 2>&1; then
      setfacl -m "u:${JAILER_USER}:rw" /dev/kvm
    else
      log "WARNING: install acl package or manually grant /dev/kvm access to $JAILER_USER"
    fi
  fi
else
  log "WARNING: /dev/kvm not present — host KVM support required"
fi

# ── TUN/TAP (optional egress) ─────────────────────────────────────────────────

if [[ -c /dev/net/tun ]]; then
  log "Granting $JAILER_USER access to /dev/net/tun"
  if command -v setfacl >/dev/null 2>&1; then
    setfacl -m "u:${JAILER_USER}:rw" /dev/net/tun
  fi
else
  log "/dev/net/tun not found — load the tun module if egress networking is needed"
fi

log "Jailer host setup complete."
echo ""
echo "  Chroot base : $CHROOT_BASE"
echo "  State dir   : $STATE_DIR"
echo "  User/group  : $JAILER_USER/$JAILER_GROUP ($JAILER_UID/$JAILER_GID)"
