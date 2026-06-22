# WSL2 Deployment & Testing Guide (Windows 11)

This is the **validated, end-to-end** path for running the full Firecracker stack —
real microVMs, KVM isolation, cgroup caps, vsock, and the HITL gate — on **WSL2**
under Windows 11. Every step here has been run and confirmed working.

> **Native Linux / bare-metal server?** Use [DEPLOY_LINUX.md](DEPLOY_LINUX.md) instead.
> **Just want the API + dashboard without KVM?** See Path A in [DEPLOY_LINUX.md](DEPLOY_LINUX.md) — it works identically inside WSL2.

---

## Why WSL2 works (and VMware/VirtualBox often don't)

WSL2 runs on Microsoft's Hyper-V layer, which **exposes `/dev/kvm` to the WSL guest**
with nested virtualization already enabled. That means Firecracker can create real
KVM VMs inside WSL2 with no extra configuration.

By contrast, on an **AMD** host with Windows 11, Hyper-V claims the CPU's
virtualization extensions and prevents VMware Player / VirtualBox from exposing
nested AMD-V to a guest. If you tried a Linux VM there and hit
`Virtualized AMD-V/RVI is not supported` or `Module 'HV' power on failed`, that's
why — WSL2 sidesteps the whole problem.

| Environment | `/dev/kvm` for Firecracker? |
|---|---|
| **WSL2 on Windows 11** | ✅ Yes (Hyper-V exposes it) |
| VMware Player on AMD + Hyper-V enabled | ❌ No (cannot nest AMD-V) |
| Bare-metal Linux | ✅ Yes |

---

## Prerequisites

### Confirm WSL2 has KVM

Open your WSL2 distro (Ubuntu 22.04+ recommended) and check:

```bash
# Kernel should be the Microsoft WSL2 kernel
uname -r
# e.g. 6.6.x-microsoft-standard-WSL2

# /dev/kvm must exist
ls -la /dev/kvm
# crw-rw---- 1 root kvm ...   ← good

# cgroup v2 unified hierarchy
stat -fc %T /sys/fs/cgroup/
# cgroup2fs   ← good (NOT tmpfs)

# systemd should be running (needed for redis service, cgroup delegation)
systemctl is-system-running
# running  (or "degraded" — both are fine)
```

If `/dev/kvm` is missing:
1. Ensure you're on **WSL2**, not WSL1: `wsl.exe -l -v` in PowerShell (VERSION must be 2).
2. Enable nested virtualization. In an **admin PowerShell** on Windows:
   ```powershell
   # In %UserProfile%\.wslconfig add:
   #   [wsl2]
   #   nestedVirtualization=true
   wsl --shutdown
   ```
   then reopen WSL2.

If `systemd` is not running, enable it in `/etc/wsl.conf`:
```ini
[boot]
systemd=true
```
then `wsl --shutdown` from PowerShell and reopen.

### Install system packages

```bash
sudo apt-get update
sudo apt-get install -y \
  git curl wget build-essential \
  redis-server \
  e2fsprogs \
  docker.io \
  python3 python3-pip \
  iproute2
```

Start Docker and Redis (the rootfs build needs Docker; the control plane needs Redis):

```bash
sudo service docker start    # or: sudo systemctl start docker
sudo service redis-server start
redis-cli ping               # → PONG
```

### Install Go 1.24

```bash
sudo rm -rf /usr/local/go
wget -q https://go.dev/dl/go1.24.0.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.24.0.linux-amd64.tar.gz
rm go1.24.0.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin          # add to ~/.bashrc to persist
go version                                   # go version go1.24.0 linux/amd64
```

### A note on the source location

Your repo lives on the Windows filesystem, mounted in WSL2 at
`/mnt/c/Users/<you>/...`. That's fine for **building source** — `go build` works
there. But **runtime artifacts go to native ext4**, not `/mnt/c`:

- VM images → `/var/lib/sandbox/`
- jailer chroots + per-VM state → `/srv/jailer`, `/srv/sandbox-state`

This matters because Firecracker hard-links drives into the chroot (needs a real
Linux filesystem) and because `/mnt/c` (DrvFs) can confuse Go's build cache — see
[Troubleshooting](#troubleshooting).

```bash
cd "/mnt/c/Users/<you>/Desktop/.../Sandboxed AI Agent Orchestration Platform"
```

---

## Step 1 — Build the guest agent (static binary)

The rootfs build bakes this in. It must be statically linked (`CGO_ENABLED=0`).

```bash
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/guest_agent ./cmd/guest-agent/
file bin/guest_agent     # ELF 64-bit ... statically linked
```

---

## Step 2 — Get the microVM kernel

The fastest reliable option is Firecracker's pre-built kernel. **Use the quickstart
`vmlinux.bin`** — the `firecracker-ci/...` URLs can return a 0-byte file with
`wget -q`.

```bash
sudo mkdir -p /var/lib/sandbox

# Known-good 21 MB pre-built kernel
wget -O /tmp/vmlinux.bin \
  https://s3.amazonaws.com/spec.ccfc.min/img/quickstart_guide/x86_64/kernels/vmlinux.bin
ls -lh /tmp/vmlinux.bin           # ~21 MB — if it's 0 bytes, re-download

sudo cp /tmp/vmlinux.bin /var/lib/sandbox/vmlinux
```

> Building a custom kernel with `scripts/build-kernel.sh` also works but takes
> 20–40 min and isn't necessary to validate the platform.

---

## Step 3 — Build the rootfs (Docker-based, ~5 min)

`build-rootfs.sh` bootstraps a minimal Alpine ext4 image with the guest agent and
an OpenRC service that auto-starts it on the vsock channel. On WSL2 (no host `apk`),
it bootstraps Alpine **via Docker** — which is why Docker must be running.

```bash
sudo bash scripts/build-rootfs.sh
ls -lh rootfs.ext4                # ~512 MB sparse ext4

sudo cp rootfs.ext4 /var/lib/sandbox/rootfs.ext4
```

Expected: `[ok] rootfs built` after ~44 Alpine packages install. The script copies
Alpine's trusted signing keys into the image before installing, so you should **not**
see `UNTRUSTED signature` errors.

---

## Step 4 — Host jailer setup

```bash
sudo bash scripts/host-jailer-setup.sh
```

This is idempotent and:
- creates `jaileruser` (uid/gid **10001**),
- creates `/srv/jailer` (owned by jaileruser) and `/srv/sandbox-state`,
- sets the jailer **setuid-root** (`chmod 4755 /usr/local/bin/jailer`),
- sets firecracker `0750 root:jaileruser` (executable by the demoted VMM),
- adds jaileruser to the `kvm` group.

You also need the Firecracker + jailer binaries installed. Download the release and
install them:

```bash
FC_VERSION="v1.10.1"
wget -q "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-x86_64.tgz"
tar -xzf "firecracker-${FC_VERSION}-x86_64.tgz"
sudo install -m 0755 "release-${FC_VERSION}-x86_64/firecracker-${FC_VERSION}-x86_64" /usr/local/bin/firecracker
sudo install -m 0755 "release-${FC_VERSION}-x86_64/jailer-${FC_VERSION}-x86_64"     /usr/local/bin/jailer

# Re-run host setup so it applies ownership/setuid to the freshly installed binaries
sudo bash scripts/host-jailer-setup.sh
firecracker --version && jailer --version
```

---

## Step 5 — Build the control plane

```bash
go build -o controlplane ./cmd/controlplane/
ls -lh controlplane
```

---

## Step 6 — Environment file

Port `8080` is often taken inside WSL2 (e.g. by Squid). This guide uses **`:7777`**;
change it if you like.

```bash
cat > .env.full << 'EOF'
# Full Firecracker stack on WSL2
USE_JAILER=true

# Firecracker binaries
FC_BIN=/usr/local/bin/firecracker
JAILER_BIN=/usr/local/bin/jailer

# VM images (note: KERNEL_IMAGE, not KERNEL_IMAGE_PATH)
KERNEL_IMAGE=/var/lib/sandbox/vmlinux
ROOTFS_IMAGE=/var/lib/sandbox/rootfs.ext4

# Auth — use a strong random value in production: openssl rand -hex 32
API_TOKEN=dev-secret-123

# Redis
REDIS_ADDR=127.0.0.1:6379
REDIS_PASSWORD=

# Server (HTTP_ADDR, not LISTEN_ADDR)
HTTP_ADDR=:7777

# Jailer isolation
JAILER_UID=10001
JAILER_GID=10001
CHROOT_BASE=/srv/jailer

# Runtime state + cgroups
STATE_DIR=/srv/sandbox-state
CGROUP_ROOT=/sys/fs/cgroup
CGROUP_BASE=sandboxes

# Admission control (0 = unlimited)
MAX_CONCURRENT=5
MEMORY_BUDGET_MIB=2048

# HITL approval window (HITL_TTL_SEC, not HITL_APPROVE_TTL)
HITL_TTL_SEC=3600
EOF
```

---

## Step 7 — Run the control plane

```bash
set -a && source .env.full && set +a
sudo -E ./controlplane            # sudo needed for jailer + cgroup writes
```

Expected:
```
{"time":"...","level":"INFO","msg":"server listening","addr":":7777"}
```

Leave this running. Open a **second terminal** for the next steps.

---

## Step 8 — Launch a real microVM

```bash
export TOKEN="dev-secret-123"
export BASE="http://localhost:7777"

curl -s -X POST $BASE/api/vms \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"vcpus": 1, "mem_mib": 256, "pids_max": 512}' | python3 -m json.tool
```

Expected — and in the control-plane logs you'll see the guest kernel boot, EXT4
mount, OpenRC start, and `Starting guest-agent ... [ ok ]`:

```json
{
  "id": "sb-xxxxxxxxxxxx",
  "cid": 3,
  "pid": 6033,
  "vcpus": 1,
  "mem_mib": 256,
  "created_at": "..."
}
```

```bash
# Confirm it's tracked
curl -s $BASE/api/vms -H "Authorization: Bearer $TOKEN" | python3 -m json.tool
```

---

## Step 9 — Exec into the guest (WebSocket terminal)

`websocat` isn't in Ubuntu's apt repos, so this repo ships a Python WebSocket test
client: `scripts/test-terminal.py`.

```bash
pip3 install --user websockets || pip3 install --user --break-system-packages websockets

PORT=7777 TOKEN=dev-secret-123 python3 scripts/test-terminal.py sb-xxxxxxxxxxxx \
  "id; uname -a; echo hello-from-guest; ls -la /workspace"
```

Expected — the command runs **as uid 1000 (agentuser)** inside the guest:
```
uid=1000(agentuser) gid=1000(agentuser)
Linux (none) 4.14.174 ... x86_64 Linux
hello-from-guest
...
[exit code=0]
```

That's the full path: WebSocket → host vsock UDS → `CONNECT 5005` → guest agent →
unprivileged exec → frames streamed back.

---

## Step 10 — Test the HITL gate (classify → hold → approve)

You'll need **three terminals**: the control plane, the client, and a free one.

**Client terminal** — send a sensitive command (held, not executed):

```bash
PORT=7777 TOKEN=dev-secret-123 python3 scripts/test-terminal.py sb-xxxxxxxxxxxx \
  "apk add curl"
```

It prints `[HITL pending] approval_id=<ID>` and the exact approve command, then waits
(the client disables keepalive pings so it can wait through a human-timescale approval).

**Free terminal** — confirm it's queued, then approve:

```bash
curl -s http://localhost:7777/api/approvals -H "Authorization: Bearer dev-secret-123" | python3 -m json.tool

curl -s -X POST http://localhost:7777/api/approvals/<ID> \
  -H "Authorization: Bearer dev-secret-123" \
  -H "Content-Type: application/json" -d '{"decision":"approve"}'
```

Back in the **client terminal**: `[HITL approved] executing...` then guest output.

> `apk add curl` will **fail** inside the guest — and that's correct. The sandbox
> boots with **none-networking** (no NIC) and runs as **uid 1000**, so apk can
> neither reach the mirrors nor lock its database. This simultaneously proves: HITL
> released the command, it really executed in the guest, network egress is blocked,
> and in-guest privilege escalation is denied.

To test **reject**, POST `{"decision":"reject"}` instead — the client prints
`[HITL rejected]` and the command never runs.

---

## Step 11 — Run the test suites

```bash
# Unit + security regression (no KVM needed)
go test ./internal/... ./cmd/... ./test/security/

# Integration: boots real microVMs (needs the images + root)
sudo -E env \
  INT_KERNEL=/var/lib/sandbox/vmlinux \
  INT_ROOTFS=/var/lib/sandbox/rootfs.ext4 \
  go test -tags=integration -v ./internal/orchestrator/ ./test/integration/

# Adversarial isolation scenarios (root; boots real VMs)
sudo FC_KERNEL=/var/lib/sandbox/vmlinux \
     FC_ROOTFS=/var/lib/sandbox/rootfs.ext4 \
     FC_BIN=/usr/local/bin/firecracker \
     bash test/adversarial/run.sh
```

---

## Verify cgroup isolation

After launching a VM with `"mem_mib": 256` and `"pids_max": 512`:

```bash
SANDBOX_ID="sb-xxxxxxxxxxxx"
cat /sys/fs/cgroup/sandboxes/${SANDBOX_ID}/memory.max   # → 268435456 (+headroom)
cat /sys/fs/cgroup/sandboxes/${SANDBOX_ID}/cpu.max      # → 100000 100000
cat /sys/fs/cgroup/sandboxes/${SANDBOX_ID}/pids.max     # → 512
```

---

## Troubleshooting

### Drive attach fails: `Error manipulating the backing file: Permission denied (os error 13) rootfs.ext4`
The per-VM rootfs copy must be group-readable/writable by the demoted VMM. This is
**fixed in the control-plane code** (the copy is `chmod 0660` + `chown root:JAILER_GID`).
If you somehow hit it, you're running a **stale binary** — rebuild to the exact path
you run (see next entry).

### Go didn't pick up a source change (stale binary)
Go's build cache can miss edits on the `/mnt/c` DrvFs mount due to timestamp quirks.
Symptoms: the error is byte-for-byte identical after a code change. Fixes:
```bash
# Build to the SAME path you run (don't build bin/x then run ./x)
go build -o controlplane ./cmd/controlplane/
# If still stale, nuke the cache:
go clean -cache && go build -o controlplane ./cmd/controlplane/
# Verify your change is compiled in (example):
grep -c "chown rootfs copy" controlplane    # 1 = present
```

### Jailer: `Failed to exec into Firecracker: Permission denied`
The firecracker binary must be executable by `jaileruser`. Re-run host setup after
installing the binaries:
```bash
sudo bash scripts/host-jailer-setup.sh
ls -la /usr/local/bin/firecracker   # -rwxr-x--- root jaileruser  (or 0755)
ls -la /usr/local/bin/jailer        # -rwsr-xr-x root root        (setuid)
```

### Port already in use
WSL2 often has Squid or another service on 8080. Pick another port:
```bash
sudo ss -ltnp | grep ':7777'        # check it's free
# change HTTP_ADDR=:7777 in .env.full
```

### `websockets` install blocked (PEP 668 externally-managed)
```bash
pip3 install --user --break-system-packages websockets
```

### HITL client drops with `keepalive ping timeout`
Use the shipped `scripts/test-terminal.py` (it sets `ping_interval=None`). A generic
WS client that pings will be disconnected while the server blocks on the approval,
because the proxy doesn't service control frames during `WaitForDecision`.

### rootfs build: `UNTRUSTED signature` or `no such package`
Ensure Docker is running. The script copies Alpine's keys and pins the main/community
repos; a stale Docker or no network will break it:
```bash
sudo service docker start
sudo bash scripts/build-rootfs.sh
```

### `vmlinux` is 0 bytes
The `firecracker-ci` S3 URLs sometimes return empty with `wget -q`. Use the
quickstart `vmlinux.bin` (Step 2) and verify it's ~21 MB.

---

## What this validates

| Layer | Verified |
|---|---|
| KVM hardware isolation | ✅ real `/dev/kvm`, guest kernel boots |
| Jailer chroot + uid/gid demotion | ✅ VMM runs as 10001 |
| cgroup v2 caps (cpu/mem/pids) | ✅ readback + admission test |
| vsock RPC → guest agent | ✅ exec round-trip |
| WebSocket ↔ vsock terminal | ✅ via test-terminal.py |
| HITL classify/hold/approve/reject | ✅ full lifecycle |
| none-networking egress block | ✅ apk can't reach mirrors |
| Unprivileged in-guest exec (uid 1000) | ✅ apk denied root |
