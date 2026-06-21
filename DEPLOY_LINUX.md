# Linux Deployment & Testing Guide

This guide gives you **two paths**:

| Path | Time | Requires KVM? | What you can test |
|---|---|---|---|
| **A — Dev mode** | ~15 min | No | API, HITL gate, React dashboard, WebSocket terminal (mock) |
| **B — Full stack** | ~90 min | Yes (`/dev/kvm`) | Real Firecracker microVMs, cgroup caps, vsock streaming, adversarial suite |

Start with Path A to verify everything compiles and the frontend works. Move to Path B once you have a KVM-capable machine.

---

## Prerequisites

### Check your Linux environment

```bash
# OS — Ubuntu 22.04+ or Debian 12+ recommended
lsb_release -a

# KVM availability (only needed for Path B)
ls -la /dev/kvm
# If this errors: you need bare-metal, WSL2 on Windows 11, or a nested-virt VM

# cgroup v2 (required for Path B)
mount | grep cgroup2
# Should show: cgroup2 on /sys/fs/cgroup type cgroup2

# Check cgroup version
stat -fc %T /sys/fs/cgroup/
# "tmpfs" = v1 (upgrade needed); "cgroup2fs" = v2 (good)
```

### Install system packages

```bash
sudo apt-get update
sudo apt-get install -y \
  git curl wget build-essential \
  redis-server \
  e2fsprogs \
  docker.io \
  iproute2 \
  nftables
```

### Install Go 1.24

```bash
# Remove any old Go installation
sudo rm -rf /usr/local/go

# Download and install Go 1.24
wget -q https://go.dev/dl/go1.24.0.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.24.0.linux-amd64.tar.gz
rm go1.24.0.linux-amd64.tar.gz

# Add to PATH (add to ~/.bashrc for persistence)
export PATH=$PATH:/usr/local/go/bin
go version   # should show: go version go1.24.0 linux/amd64
```

### Install Node.js 20 LTS

```bash
curl -fsSL https://deb.nodesource.com/setup_20.x | sudo -E bash -
sudo apt-get install -y nodejs
node --version   # v20.x.x
npm --version    # 10.x.x
```

---

## Step 0 — Clone and enter the repo

```bash
git clone https://github.com/nk1947-sudo/Sandboxed-AI-Agent-Orchestration-Platform.git
cd Sandboxed-AI-Agent-Orchestration-Platform
```

---

## PATH A — Dev Mode (no KVM required)

This mode skips Firecracker entirely. The control plane starts, Redis runs, and the HITL gate and all API endpoints work. The `/api/vms` endpoint returns an error when you try to actually launch a VM (there is no VMM), but everything else — authentication, HITL approval/reject, WebSocket connection, and the full React dashboard — is fully testable.

### A1 — Start Redis

```bash
sudo systemctl start redis-server
redis-cli ping     # → PONG
```

### A2 — Download Go modules and run portable tests

```bash
# Download all dependencies
go mod download

# Run portable unit tests (work on any OS/arch including no KVM)
go test -v ./internal/hitl/ ./internal/protocol/ ./internal/state/
```

Expected: all tests pass. These cover the HITL classifier, protocol framing, and Redis state layer.

### A3 — Cross-compile check (catches Linux-only build errors)

```bash
GOOS=linux GOARCH=amd64 go build ./...
GOOS=linux GOARCH=amd64 go vet ./...
```

Expected: no errors, no warnings.

### A4 — Build the control plane binary

```bash
go build -o controlplane ./cmd/controlplane/
ls -lh controlplane
```

### A5 — Create a dev environment file

```bash
cat > .env.dev << 'EOF'
# Dev mode — no Firecracker, no jailer, no KVM required
USE_JAILER=false
API_TOKEN=dev-secret-123

# Redis (local)
REDIS_ADDR=127.0.0.1:6379
REDIS_PASSWORD=

# These paths don't need to exist in dev mode (no real VMs will boot)
KERNEL_IMAGE=/tmp/vmlinux-placeholder
ROOTFS_IMAGE=/tmp/rootfs-placeholder

# Server
HTTP_ADDR=:8080

# State directories (created automatically)
STATE_DIR=/tmp/sandbox-state
CHROOT_BASE=/tmp/jailer

# HITL approval window
HITL_TTL_SEC=3600
EOF
```

### A6 — Run the control plane

```bash
# Load env and run (no sudo needed in dev mode)
set -a && source .env.dev && set +a
./controlplane
```

You should see:
```
{"time":"...","level":"INFO","msg":"server listening","addr":":8080"}
```

Leave this running. Open a **second terminal** for the next steps.

### A7 — Verify the API

```bash
export TOKEN="dev-secret-123"
export BASE="http://localhost:8080"

# Health check
curl -s $BASE/healthz          # → ok
curl -s $BASE/readyz           # → {"active":0}

# Auth check — wrong token should get 401
curl -s -o /dev/null -w "%{http_code}" $BASE/api/vms
# → 401 (no token)

curl -s -H "Authorization: Bearer $TOKEN" $BASE/api/vms
# → [] (empty list — no VMs running in dev mode)

# HITL approvals list
curl -s -H "Authorization: Bearer $TOKEN" $BASE/api/approvals | python3 -m json.tool
```

### A8 — Build and start the React dashboard

```bash
cd web
npm install
npm run dev
# → http://localhost:3000 (or :3001 if port is taken)
```

Open `http://localhost:3000` in a browser:
1. Enter token: `dev-secret-123`
2. Click **Sign in**
3. You should see the VM list (empty) and Approval Wall

### A9 — Security regression tests (Linux, no KVM)

```bash
# Back in the repo root (new terminal)
go test -v -race ./test/security/
```

Expected: 12 tests pass, covering frame-size cap (R1), WS origin allowlist (R2), constant-time auth (R3), and cp/mv classifier (R4).

**That's Path A complete.** You've verified: Redis connectivity, API auth, HITL gate, security regressions, and the React dashboard.

---

## PATH B — Full Stack with Firecracker microVMs

### B1 — Download Firecracker and jailer

```bash
# Check latest release at https://github.com/firecracker-microvm/firecracker/releases
FC_VERSION="v1.10.1"

# Download the x86_64 binaries
wget -q "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-x86_64.tgz"
tar -xzf "firecracker-${FC_VERSION}-x86_64.tgz"

# Install to /usr/local/bin
sudo mv "release-${FC_VERSION}-x86_64/firecracker-${FC_VERSION}-x86_64" /usr/local/bin/firecracker
sudo mv "release-${FC_VERSION}-x86_64/jailer-${FC_VERSION}-x86_64"     /usr/local/bin/jailer
sudo chmod +x /usr/local/bin/firecracker /usr/local/bin/jailer

# Verify
firecracker --version
jailer --version
```

### B2 — Build the guest agent (static binary)

The rootfs build script expects the guest agent at `bin/guest_agent`.

```bash
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -o bin/guest_agent ./cmd/guest-agent/

ls -lh bin/guest_agent   # Should be ~5 MB, statically linked
file bin/guest_agent     # ELF 64-bit LSB executable, statically linked
```

### B3 — Build the microVM kernel (~20–40 min)

The kernel build downloads Linux 6.1 source and compiles a minimal vmlinux. This only needs to be done once.

```bash
# Install kernel build dependencies
sudo apt-get install -y \
  libncurses-dev bison flex libssl-dev libelf-dev

# Build (takes 20-40 minutes depending on your CPU)
bash scripts/build-kernel.sh

# Output: vmlinux-6.1 in the current directory
ls -lh vmlinux-6.1

# Move to the standard location
sudo mkdir -p /var/lib/sandbox
sudo cp vmlinux-6.1 /var/lib/sandbox/vmlinux-6.1
```

**Shortcut:** If you have a pre-built Firecracker vmlinux from the Firecracker team:

```bash
# Firecracker provides pre-built kernels for testing
wget -q "https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/x86_64/vmlinux-5.10.225"
sudo mkdir -p /var/lib/sandbox
sudo cp vmlinux-5.10.225 /var/lib/sandbox/vmlinux-6.1
```

### B4 — Build the rootfs (~5 min)

```bash
# Requires root (loop mount)
sudo bash scripts/build-rootfs.sh

# Output: rootfs.ext4 in the current directory
ls -lh rootfs.ext4

# Move to the standard location
sudo cp rootfs.ext4 /var/lib/sandbox/rootfs.ext4
sudo chmod 444 /var/lib/sandbox/rootfs.ext4   # golden image — read-only
```

### B5 — Host setup (jaileruser, cgroups, paths)

```bash
sudo bash scripts/host-jailer-setup.sh
```

This script:
- Creates `jaileruser` (uid/gid 10001)
- Creates `/srv/jailer` and `/srv/sandbox-state` with correct permissions
- Sets the jailer binary setuid-root: `chmod 4755 /usr/local/bin/jailer`
- Adds your user to the `kvm` group: `sudo usermod -aG kvm $USER`
- Creates a dedicated cgroup subtree: `/sys/fs/cgroup/sandboxes`

```bash
# Apply the group change without logging out
newgrp kvm

# Verify KVM access
ls -la /dev/kvm
# crw-rw---- 1 root kvm ...   ← you need to be in the kvm group

# Verify cgroup subtree
ls /sys/fs/cgroup/sandboxes
```

### B6 — Apply nftables firewall rules

```bash
sudo nft -f deploy/firewall/sandbox.nft

# Verify rules are loaded
sudo nft list ruleset | grep sandbox
```

### B7 — Build the control plane

```bash
go build -o controlplane ./cmd/controlplane/
```

### B8 — Create the full-stack environment file

```bash
cat > .env.full << 'EOF'
# Full Firecracker stack
USE_JAILER=true

# Firecracker binaries
FC_BIN=/usr/local/bin/firecracker
JAILER_BIN=/usr/local/bin/jailer

# VM images
KERNEL_IMAGE=/var/lib/sandbox/vmlinux-6.1
ROOTFS_IMAGE=/var/lib/sandbox/rootfs.ext4

# Auth (change this to something stronger in production)
API_TOKEN=dev-secret-123

# Redis
REDIS_ADDR=127.0.0.1:6379
REDIS_PASSWORD=

# Server
HTTP_ADDR=:8080

# Jailer isolation
JAILER_UID=10001
JAILER_GID=10001
CHROOT_BASE=/srv/jailer

# Runtime state
STATE_DIR=/srv/sandbox-state
CGROUP_ROOT=/sys/fs/cgroup
CGROUP_BASE=sandboxes

# Resource limits (0 = unlimited in dev; set real values in production)
MAX_CONCURRENT=5
MEMORY_BUDGET_MIB=2048

# HITL
HITL_TTL_SEC=3600
EOF
```

### B9 — Run the full control plane

```bash
# Load env
set -a && source .env.full && set +a

# Requires sudo for jailer + cgroup writes
sudo -E ./controlplane
```

You should see:
```
{"level":"INFO","msg":"server listening","addr":":8080"}
```

### B10 — Launch a real microVM

In a second terminal:

```bash
export TOKEN="dev-secret-123"
export BASE="http://localhost:8080"

# Launch a sandbox VM (1 vCPU, 256 MiB RAM, max 512 pids)
curl -s -X POST $BASE/api/vms \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"vcpus": 1, "mem_mib": 256, "pids_max": 512}' | python3 -m json.tool
```

Expected response:
```json
{
  "id": "sb-xxxxxxxx",
  "cid": 3,
  "pid": 12345,
  "vcpus": 1,
  "mem_mib": 256,
  "created_at": "2026-06-21T10:00:00Z"
}
```

```bash
# List running VMs
curl -s -H "Authorization: Bearer $TOKEN" $BASE/api/vms | python3 -m json.tool

# Connect to the terminal (install websocat for WS testing)
sudo apt-get install -y websocat   # or: cargo install websocat

SANDBOX_ID="sb-xxxxxxxx"   # replace with actual ID from launch response
websocat "ws://localhost:8080/terminal?sandbox=${SANDBOX_ID}&token=${TOKEN}"

# Once connected, type commands:
echo "hello from inside the microVM"
uname -a
ls /workspace
```

### B11 — Test the HITL gate

```bash
SANDBOX_ID="sb-xxxxxxxx"

# This command will be intercepted by the HITL classifier
websocat "ws://localhost:8080/terminal?sandbox=${SANDBOX_ID}&token=${TOKEN}"
# Type: curl https://example.com
# The terminal pauses with a "pending" event
```

In another terminal, approve or reject it:
```bash
# List pending approvals
curl -s -H "Authorization: Bearer $TOKEN" $BASE/api/approvals | python3 -m json.tool

# Approve (use the ID from the list above)
APPROVAL_ID="ap-xxxxxxxx"
curl -s -X POST $BASE/api/approvals/${APPROVAL_ID} \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"decision": "approve"}'

# Or reject
curl -s -X POST $BASE/api/approvals/${APPROVAL_ID} \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"decision": "reject"}'
```

### B12 — Run the full integration test suite

```bash
sudo INT_KERNEL=/var/lib/sandbox/vmlinux-6.1 \
     INT_ROOTFS=/var/lib/sandbox/rootfs.ext4 \
     go test -v -tags=integration -timeout=10m \
     ./internal/orchestrator/ ./test/integration/
```

### B13 — Run the adversarial security scenarios

```bash
# Must run as root; these boot real VMs and verify isolation holds
sudo FC_KERNEL=/var/lib/sandbox/vmlinux-6.1 \
     FC_ROOTFS=/var/lib/sandbox/rootfs.ext4 \
     FC_BIN=/usr/local/bin/firecracker \
     bash test/adversarial/run.sh
```

Expected output:
```
[PASS] 01: fork-bomb — pids.max enforced, host load bounded
[PASS] 02: dir-traversal — /etc/shadow not readable from guest
[PASS] 03: network-exfil — IMDS connection refused
```

---

## Starting Everything Together (Quick Reference)

Once you've completed the setup steps above, use this to start the full stack:

**Terminal 1 — Redis**
```bash
sudo systemctl start redis-server
```

**Terminal 2 — Control Plane**
```bash
cd /path/to/Sandboxed-AI-Agent-Orchestration-Platform
set -a && source .env.full && set +a
sudo -E ./controlplane
```

**Terminal 3 — Web Dashboard**
```bash
cd /path/to/Sandboxed-AI-Agent-Orchestration-Platform/web
npm run dev
# Open http://localhost:3000
```

---

## Troubleshooting

### "redis unreachable"
```bash
sudo systemctl status redis-server
sudo systemctl start redis-server
redis-cli ping    # must return PONG
```

### "failed to open /dev/kvm: permission denied"
```bash
sudo usermod -aG kvm $USER
newgrp kvm          # apply without logout
ls -la /dev/kvm     # should show kvm group
```

### "cgroup: no such file or directory" at /sys/fs/cgroup/sandboxes
```bash
# Create the cgroup subtree manually
sudo mkdir -p /sys/fs/cgroup/sandboxes
sudo chown $USER:$USER /sys/fs/cgroup/sandboxes
# Then enable controllers
echo "+cpu +memory +pids" | sudo tee /sys/fs/cgroup/cgroup.subtree_control
```

### "cannot unmarshal" or "connection refused" on WS terminal
The VM hasn't fully booted yet. Firecracker boots in ~800 ms. Wait a moment and try again. Check control plane logs for vsock connection errors.

### rootfs build fails: "apk not found"
Ensure Docker is running — the build script falls back to Docker to bootstrap Alpine:
```bash
sudo systemctl start docker
sudo bash scripts/build-rootfs.sh
```

### "jailer: permission denied" or setuid issues
```bash
ls -la /usr/local/bin/jailer
# Should show: -rwsr-xr-x (setuid bit set)

# If not set:
sudo chmod 4755 /usr/local/bin/jailer
```

### Control plane exits immediately with "init supervisor" error (dev mode)
In dev mode (`USE_JAILER=false`), the kernel and rootfs paths don't need to exist. If you still see this error, check that `STATE_DIR` is writable:
```bash
mkdir -p /tmp/sandbox-state
export STATE_DIR=/tmp/sandbox-state
```

### npm: EACCES permission errors
```bash
mkdir ~/.npm-global
npm config set prefix '~/.npm-global'
export PATH=~/.npm-global/bin:$PATH
```

### Port 8080 already in use
```bash
sudo lsof -i :8080
# Kill the conflicting process, or change HTTP_ADDR=:8081 in your .env file
```

---

## Verifying Cgroup Isolation

After launching a VM, confirm cgroup limits are applied:

```bash
# Get the VM's cgroup path (printed in control plane logs as "cgroup_path")
SANDBOX_ID="sb-xxxxxxxx"

# Or find it:
cat /sys/fs/cgroup/sandboxes/${SANDBOX_ID}/memory.max
cat /sys/fs/cgroup/sandboxes/${SANDBOX_ID}/cpu.max
cat /sys/fs/cgroup/sandboxes/${SANDBOX_ID}/pids.max
```

For a VM launched with `"mem_mib": 256` and `"pids_max": 512`:
```
memory.max → 268435456   (256 * 1024 * 1024 bytes)
cpu.max    → 100000 100000
pids.max   → 512
```

---

## Test Coverage Summary

| Test suite | Command | Requires KVM |
|---|---|---|
| Portable unit tests | `go test ./internal/hitl/ ./internal/protocol/ ./internal/state/` | No |
| Security regressions (R1–R4) | `go test -v ./test/security/` | No |
| Full unit suite with race detector | `go test -race ./...` | No |
| VM lifecycle integration | `go test -tags=integration ./internal/orchestrator/ ./test/integration/` | Yes |
| Load & concurrency | `go test -tags=integration ./test/load/` | Yes |
| Snapshot restore benchmarks | `go test -tags=integration ./test/bench/` | Yes |
| Adversarial security (fork-bomb, traversal, exfil) | `bash test/adversarial/run.sh` | Yes (as root) |
