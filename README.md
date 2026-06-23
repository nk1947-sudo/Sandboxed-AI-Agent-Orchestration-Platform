# Sandboxed AI Agent Orchestration Platform

> **Hardware-isolated, human-supervised execution of untrusted AI agent code — with a real-time web dashboard, a HITL approval gate, and sub-125 ms CoW snapshot restore.**

Every agent runs inside its own Firecracker microVM (a full guest kernel over KVM), not a container. The host filesystem, network stack, and credential metadata are structurally unreachable — not filtered by string patterns. A built-in Human-in-the-Loop (HITL) gate intercepts sensitive commands before they execute and holds them for operator approval through the web UI.

### 📦 Deployment guides

| Platform | Guide | What it covers |
|---|---|---|
| **Windows 11 + WSL2** | **[DEPLOY_WSL2.md](DEPLOY_WSL2.md)** | Validated end-to-end: real Firecracker microVMs over WSL2's Hyper-V `/dev/kvm`, exact image URLs, ports, and gotchas. |
| **Native / bare-metal Linux** | **[DEPLOY_LINUX.md](DEPLOY_LINUX.md)** | Two paths — dev mode (no KVM) and the full Firecracker stack — for Linux servers and nested-virt VMs. |

The [Getting Started](#getting-started--local-development) section below is the condensed quick-start; use the guides above for a step-by-step, copy-paste deployment.

---

## Table of Contents

- [Key Features](#key-features)
- [Architecture Overview](#architecture-overview)
- [Trust Boundaries](#trust-boundaries)
- [Prerequisites](#prerequisites)
- [Getting Started — Local Development](#getting-started--local-development)
- [Environment Variables](#environment-variables)
- [Production Deployment](#production-deployment)
- [API Reference](#api-reference)
- [Security Hardening Guide](#security-hardening-guide)
- [Running the Test Suite](#running-the-test-suite)
- [Project Structure](#project-structure)

---

## Key Features

| Feature | Detail |
|---|---|
| **KVM microVM isolation** | Every sandbox is a full Firecracker guest with its own kernel. Container-escape via shared syscalls is impossible by construction. |
| **Human-in-the-Loop (HITL) gate** | A Redis-backed classifier intercepts `sudo`, `curl`, `pip install`, destructive `rm`, writes to system paths, and more — streaming a real-time approval/reject event to the operator dashboard before the script runs. |
| **WebSocket streaming terminal** | xterm.js in the browser connects over a multiplexed vsock channel directly into the guest agent. Output streams frame-by-frame; no polling. |
| **CoW snapshot engine** | Capture a golden VM state once; restore N sandboxes from it in under 125 ms using overlayfs (no full rootfs copy per VM). A pre-warmed pool keeps slots ready for sub-millisecond `Acquire()`. |
| **Default-deny networking** | Sandboxes boot with no TAP interface. Opt-in egress routes through an nftables default-deny chain and a Squid forward proxy enforcing a `dstdomain` allowlist. IMDS (`169.254.169.254`) is blocked at both layers. |
| **Defense-in-depth process isolation** | VMM runs inside the Firecracker jailer (chroot + PID namespace + uid demotion). Each VMM gets a dedicated cgroup v2 leaf with hard `cpu.max`, `memory.max`, `memory.swap.max=0`, and `pids.max` caps applied **before** boot. seccomp-BPF filters restrict the VMM's syscall surface to ~60 calls. |
| **React operator dashboard** | Tailwind + xterm.js UI: live VM list, terminal sessions, and an approval wall for pending HITL decisions. Token auth stored in JS module memory (never `localStorage`). |
| **Adversarial test suite** | Three Phase-8 scenarios — fork-bomb, directory-traversal escape, and network exfiltration — each verified to fail against the isolation controls. CI gates integration and nightly adversarial runs on a self-hosted KVM runner. |

---

## Architecture Overview

```
┌──────────────────────────── BARE-METAL HOST ──────────────────────────────┐
│                                                                            │
│  React Dashboard ──── WS ────► Go Control Plane                           │
│  (Tailwind, xterm.js)          (API + WS proxy)                           │
│        ▲  HITL approvals              │                                    │
│        │                             │ vsock UDS  CONNECT 5005             │
│        │         ┌───────────────────▼───────────────────────────────┐    │
│        │         │  jailer chroot  (uid demoted, PID ns, seccomp)    │    │
│        │         │  ┌─────────────────────────────────────────────┐  │    │
│        │         │  │  Firecracker VMM — cgroup v2                │  │    │
│        │         │  │  ┌───────────────────────────────────────┐  │  │    │
│        │         │  │  │  KVM guest kernel                     │  │  │    │
│        │         │  │  │  Go guest agent (vsock:5005)          │  │  │    │
│        │         │  │  │  └─► bash/python  (uid 1000)          │  │  │    │
│        │         │  │  └───────────────────────────────────────┘  │  │    │
│        │         │  └─────────────────────────────────────────────┘  │    │
│        │         └───────────────────────────────────────────────────┘    │
│        │                                                                   │
│  Redis (HITL queue, state, rate-limit, heartbeats)                        │
│                                                                            │
│  Optional egress: TAP ─► nftables default-deny ─► Squid allowlist         │
└────────────────────────────────────────────────────────────────────────────┘
```

**Control flow for a terminal command:**

1. Browser sends input over WS to the control plane.
2. The HITL classifier checks the script against ~15 rule patterns (network tools, privilege escalation, package managers, writes to system paths, destructive `rm`).
3. **If sensitive:** the gate submits an approval request to Redis, streams a `pending` event to the browser, and blocks until an operator approves or rejects from the dashboard.
4. **If benign (or approved):** the control plane dials the sandbox's vsock UDS, sends a length-prefixed JSON `ExecutionRequest`, and streams `ResponseFrame` chunks back to the terminal.

---

## Trust Boundaries

| Boundary | Inside (untrusted) | Outside (trusted) | Enforced by |
|---|---|---|---|
| B1 | User script | Guest agent | uid 1000, `/workspace` confinement, process-group kill |
| B2 | Guest userland | Guest kernel | Linux DAC |
| B3 | Guest kernel | VMM | KVM / Firecracker device model |
| B4 | VMM | Host kernel | jailer (chroot, PID ns, uid demotion), seccomp-BPF |
| B5 | VMM resource use | Host resources | cgroup v2 (`cpu.max`, `memory.max`, `pids.max`) |
| B6 | Guest network | Host LAN / internet | none-networking default; opt-in TAP → Squid allowlist |
| B7 | Operator client | Control plane | Bearer auth (constant-time compare), WS origin allowlist, rate limit, HITL |

---

## Prerequisites

### Control Plane (Linux host required)

| Requirement | Version | Notes |
|---|---|---|
| Linux kernel | ≥ 5.10 | cgroup v2 unified hierarchy |
| KVM | — | `/dev/kvm` must be present |
| Go | ≥ 1.24 | `go build ./...` |
| Firecracker | ≥ 1.5 | [GitHub releases](https://github.com/firecracker-microvm/firecracker/releases) |
| Firecracker jailer | same release as FC | ships alongside the FC binary |
| Redis | ≥ 7.0 | HITL queue, state, rate-limiting |
| vmlinux | 5.10 microVM build | see `scripts/build-kernel.sh` |
| rootfs.ext4 | Alpine-based | see `scripts/build-rootfs.sh` |

### Web Dashboard (any OS)

| Requirement | Version |
|---|---|
| Node.js | ≥ 20 LTS |
| npm | ≥ 10 |

---

## Getting Started — Local Development

### 1. Clone

```bash
git clone https://github.com/nk1947-sudo/Sandboxed-AI-Agent-Orchestration-Platform.git
cd Sandboxed-AI-Agent-Orchestration-Platform
```

### 2. Build the kernel and rootfs (Linux only)

```bash
# Install build deps (Debian/Ubuntu)
sudo apt install -y build-essential libncurses-dev bison flex libssl-dev \
  libelf-dev squashfs-tools debootstrap qemu-utils

# Build the microVM kernel (~20–40 min) — or use a pre-built vmlinux (see deploy guides)
bash scripts/build-kernel.sh

# Build the Alpine rootfs with the Go guest agent baked in (~5 min; needs Docker or apk)
sudo bash scripts/build-rootfs.sh
```

Output: `vmlinux` and `rootfs.ext4` (copy them to `/var/lib/sandbox/`).

### 3. Host setup (production jailer mode)

```bash
# Create jaileruser, set up paths, configure KVM ACL, set capabilities
sudo bash scripts/host-jailer-setup.sh

# Compile and install seccomp-BPF filters
bash scripts/build-seccomp.sh
```

### 4. Start Redis

```bash
# Docker (quickest)
docker run -d --name sandbox-redis -p 6379:6379 redis:7-alpine

# Or system Redis
sudo systemctl start redis
```

### 5. Configure environment

```bash
cp .env.example .env   # then edit with your values
```

See [Environment Variables](#environment-variables) for the full reference.

### 6. Run the control plane

```bash
go build -o controlplane ./cmd/controlplane/
sudo ./controlplane      # sudo needed for jailer + cgroup writes
```

**Dev mode** (no jailer, no real VMs — HITL and API still work):

```bash
USE_JAILER=false API_TOKEN=dev ./controlplane
```

### 7. Start the web dashboard

```bash
cd web
npm install
npm run dev        # http://localhost:3000
```

Open `http://localhost:3000`, enter your `API_TOKEN`, and sign in.

---

## Environment Variables

Copy `.env.example` to `.env` and populate the values below. The control plane reads all configuration from environment; there is no config file.

> Variable names below match `loadConfig()` in `cmd/controlplane/main.go` exactly.

### Required

| Variable | Description | Example |
|---|---|---|
| `API_TOKEN` | Bearer token for all API and WS connections. Empty = auth disabled (dev only). | `change-me-in-production` |
| `REDIS_ADDR` | Redis address for HITL queue, state, and rate-limiting. | `127.0.0.1:6379` |
| `KERNEL_IMAGE` | Host path to the built microVM kernel (`vmlinux`). | `/var/lib/sandbox/vmlinux` |
| `ROOTFS_IMAGE` | Host path to the golden rootfs image (`rootfs.ext4`). | `/var/lib/sandbox/rootfs.ext4` |

Defaults: `KERNEL_IMAGE=/var/lib/sandbox/vmlinux-6.1`, `ROOTFS_IMAGE=/var/lib/sandbox/rootfs.ext4`.

### Jailer & Isolation

| Variable | Description | Default |
|---|---|---|
| `USE_JAILER` | `true` in production; `false` for dev without root. | `true` |
| `FC_BIN` | Path to the firecracker binary. | `firecracker` |
| `JAILER_BIN` | Path to the jailer binary. | `jailer` |
| `CHROOT_BASE` | Base directory for per-VM jailer chroots. | `/srv/jailer` |
| `JAILER_UID` | Unprivileged uid the VMM is demoted to. | `10001` |
| `JAILER_GID` | Unprivileged gid the VMM is demoted to. | `10001` |
| `STATE_DIR` | Per-VM runtime state (sockets, rootfs copies). | `/srv/sandbox-state` |
| `CGROUP_ROOT` | cgroup v2 mount point. | `/sys/fs/cgroup` |
| `CGROUP_BASE` | Parent cgroup leaf for all sandboxes. | `sandboxes` |

### Admission Control

| Variable | Description | Default |
|---|---|---|
| `MAX_CONCURRENT` | Maximum simultaneously running sandboxes. `0` = unlimited. | `0` |
| `MEMORY_BUDGET_MIB` | Total guest RAM the host will commit across all VMs. `0` = unlimited. | `0` |

### Gateway & API

| Variable | Description | Default |
|---|---|---|
| `HTTP_ADDR` | HTTP/WS listen address. | `:8080` |
| `ALLOWED_ORIGINS` | Comma-separated WebSocket CORS allowlist. Empty = same-host only. | *(empty)* |

### Redis, HITL & Runtime

| Variable | Description | Default |
|---|---|---|
| `REDIS_PASSWORD` | Redis auth password (set with `requirepass`). | *(empty)* |
| `REDIS_DB` | Redis logical database index. | `0` |
| `HITL_TTL_SEC` | Seconds an approval request stays pending before it expires. | `3600` |
| `VSOCK_RETRY_MAX` | Max vsock dial retries while a guest finishes booting. | `10` |
| `HEARTBEAT_TTL_SEC` | Sandbox heartbeat TTL in Redis. | `15` |
| `HEALTH_INTERVAL_SEC` | Reaper / health-loop interval. | `5` |
| `SHUTDOWN_TERMINATES_ALL` | Terminate all running VMs on graceful shutdown. | `true` |

### PostgreSQL & Accounts (optional)

Enables user accounts, persistent login sessions, durable sandbox history,
true resume (memory + disk), and saved transcripts. Leave `PG_HOST` empty to run
without it. See [DEPLOY_WSL2.md](DEPLOY_WSL2.md) for the full setup.

| Variable | Description | Default |
|---|---|---|
| `PG_HOST` | Postgres host. **Empty disables the whole data layer.** | *(empty)* |
| `PG_PORT` / `PG_USER` / `PG_PASSWORD` / `PG_DB` | Connection parameters. | `5432` / `postgres` / *(empty)* / `sandbox` |
| `PG_SSLMODE` | `disable` (dev) … `verify-full` (prod). | `disable` |
| `ADMIN_USER` / `ADMIN_PASSWORD` | First admin, created when the users table is empty. | *(empty)* |
| `SESSION_TTL_HOURS` | Login session lifetime. | `24` |
| `COOKIE_SECURE` | Set `true` behind HTTPS (Secure session cookie). | `false` |
| `TRANSCRIPT_RETENTION_DAYS` | Days to keep terminal transcripts. | `30` |

> The session cookie holds an opaque session id, **not** the API token — the
> token is never written to browser storage.

---

## Production Deployment

### Docker Compose

```yaml
# docker-compose.yml
version: "3.9"

services:
  redis:
    image: redis:7-alpine
    restart: unless-stopped
    volumes:
      - redis-data:/data
    command: redis-server --appendonly yes

  controlplane:
    build: .
    restart: unless-stopped
    privileged: true                   # required for KVM + jailer + cgroup writes
    devices:
      - /dev/kvm:/dev/kvm
    volumes:
      - /srv/jailer:/srv/jailer
      - /srv/sandbox-state:/srv/sandbox-state
      - /srv/images:/srv/images:ro
      - /sys/fs/cgroup:/sys/fs/cgroup:rw
    environment:
      - API_TOKEN=${API_TOKEN}
      - REDIS_ADDR=redis:6379
      - KERNEL_IMAGE=/srv/images/vmlinux
      - ROOTFS_IMAGE=/srv/images/rootfs.ext4
      - USE_JAILER=true
      - HTTP_ADDR=:8080
      - ALLOWED_ORIGINS=https://dashboard.yourdomain.com
    depends_on:
      - redis
    ports:
      - "127.0.0.1:8080:8080"   # expose only to localhost; put nginx in front

  dashboard:
    build:
      context: web
    restart: unless-stopped
    ports:
      - "127.0.0.1:3000:3000"

volumes:
  redis-data:
```

```bash
docker compose up -d
```

### Nginx Reverse Proxy (TLS termination)

```nginx
server {
    listen 443 ssl http2;
    server_name api.yourdomain.com;

    ssl_certificate     /etc/letsencrypt/live/yourdomain.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/yourdomain.com/privkey.pem;

    location / {
        proxy_pass         http://127.0.0.1:8080;
        proxy_http_version 1.1;

        # Required for WebSocket upgrade
        proxy_set_header Upgrade    $http_upgrade;
        proxy_set_header Connection "Upgrade";
        proxy_set_header Host       $host;

        # Long timeout for streaming terminal sessions
        proxy_read_timeout 3600s;
    }
}
```

### systemd Service

```bash
sudo cp deploy/systemd/controlplane.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now controlplane
```

### Firewall

```bash
sudo nft -f deploy/firewall/sandbox.nft
```

Enforces: IMDS drop → sandbox→proxy allow → all other sandbox egress drop+log.

### Egress Proxy

```bash
sudo apt install -y squid
sudo cp deploy/squid/squid.conf /etc/squid/squid.conf
sudo systemctl restart squid
```

Default allowlist: `openai.com`, `anthropic.com`, `huggingface.co`, `googleapis.com`, `github.com`, `pypi.org`, `npmjs.com`. Raw IPs and RFC-1918 ranges are always denied.

---

## API Reference

All endpoints require `Authorization: Bearer <API_TOKEN>` unless `API_TOKEN` is unset.

### Launch a sandbox

```bash
curl -s -X POST http://localhost:8080/api/vms \
  -H "Authorization: Bearer dev" \
  -H "Content-Type: application/json" \
  -d '{"vcpus": 1, "mem_mib": 256, "pids_max": 512}' | jq .
```

```json
{
  "id": "sb-a3f9c2",
  "cid": 5,
  "pid": 18423,
  "vcpus": 1,
  "mem_mib": 256,
  "created_at": "2026-06-21T10:00:00Z"
}
```

### List running sandboxes

```bash
curl -s http://localhost:8080/api/vms \
  -H "Authorization: Bearer dev" | jq .
```

### Terminate a sandbox

```bash
curl -s -X DELETE http://localhost:8080/api/vms/sb-a3f9c2 \
  -H "Authorization: Bearer dev"
# → 204 No Content
```

### Open a streaming terminal (WebSocket)

Auth is the `Authorization: Bearer` header (not a query param). The repo ships a
Python client so you don't need `websocat`:

```bash
# Shipped client — runs a script in the guest and streams output/HITL events
PORT=8080 TOKEN=dev python3 scripts/test-terminal.py sb-a3f9c2 "uname -a; id"

# Or with websocat (cargo install websocat):
websocat -H "Authorization: Bearer dev" "ws://localhost:8080/terminal?sandbox=sb-a3f9c2"
```

### List pending HITL approvals

```bash
curl -s http://localhost:8080/api/approvals \
  -H "Authorization: Bearer dev" | jq .
```

```json
{
  "approvals": [
    {
      "id": "ap-7d2e1f",
      "sandbox_id": "sb-a3f9c2",
      "script": "pip install requests",
      "reason": "pip",
      "state": "pending",
      "submitted_at": "2026-06-21T10:01:23Z"
    }
  ],
  "count": 1
}
```

### Approve or reject a HITL request

```bash
# Approve
curl -s -X POST http://localhost:8080/api/approvals/ap-7d2e1f \
  -H "Authorization: Bearer dev" \
  -H "Content-Type: application/json" \
  -d '{"decision": "approve"}'

# Reject
curl -s -X POST http://localhost:8080/api/approvals/ap-7d2e1f \
  -H "Authorization: Bearer dev" \
  -H "Content-Type: application/json" \
  -d '{"decision": "reject"}'
```

### Health & readiness

```bash
curl http://localhost:8080/healthz   # → 200 OK
curl http://localhost:8080/readyz    # → 200 OK once Redis is connected
```

---

## Security Hardening Guide

### Production checklist

- [ ] Set a strong, random `API_TOKEN` — e.g. `openssl rand -hex 32`
- [ ] Set `ALLOWED_ORIGINS` to your dashboard's exact origin. Do **not** leave empty in production.
- [ ] Enable `USE_JAILER=true` — applies chroot, PID namespace, uid/gid demotion, and seccomp-BPF.
- [ ] Apply seccomp-BPF filters via `bash scripts/build-seccomp.sh`.
- [ ] Apply nftables rules via `sudo nft -f deploy/firewall/sandbox.nft`.
- [ ] Deploy Squid with `deploy/squid/squid.conf`; restrict `dstdomain` to only what agents need.
- [ ] Set `MEMORY_BUDGET_MIB` and `MAX_CONCURRENT` to prevent host OOM.
- [ ] Always pass `pids_max` in launch requests (e.g. `512`) to cap fork-bomb blast radius.
- [ ] Run `sudo bash test/adversarial/run.sh` after any infrastructure change to verify all three isolation scenarios still hold.
- [ ] Put TLS termination (nginx/Caddy) in front of the control plane — never expose port 8080 directly.
- [ ] Enable Redis `requirepass` and supply credentials in `REDIS_ADDR`.

### What the HITL gate intercepts

| Category | Example triggers |
|---|---|
| Package managers | `pip install`, `npm install`, `apt-get`, `apk add`, `yum install` |
| Network tools | `curl`, `wget`, `ssh`, `nc`, `nmap` |
| Privilege escalation | `sudo`, `doas`, `su` |
| System path writes | `tee /etc/…`, `cp … /usr/bin/`, `> /etc/passwd` |
| Destructive `rm` | `rm -rf /var/…`, `rm -fr /` |
| Container tools | `docker`, `kubectl`, `podman` |
| Ownership / mode changes | `chown root …`, `chmod 777 /etc/…` |

> Writes to `/workspace` are always benign and bypass the gate.

---

## Running the Test Suite

### Portable unit tests (any OS)

```bash
go test ./...
```

Covers: protocol framing, frame-size cap (`MaxFrameSize = 16 MiB`), HITL classify/approve/reject/wait, Redis state layer, rate-limit token bucket.

### Security regression tests (Linux, no KVM needed)

```bash
go test -v ./test/security/
```

Covers: oversized frame rejection, WS origin allowlist enforcement, constant-time auth comparison, cp/mv destination classifier regression.

### Integration tests (Linux + `/dev/kvm`)

```bash
INT_KERNEL=/path/to/vmlinux INT_ROOTFS=/path/to/rootfs.ext4 \
  go test -v -tags=integration -timeout=10m \
  ./internal/orchestrator/ ./test/integration/ ./test/bench/ ./test/load/
```

Covers: VM lifecycle, cgroup cap readback, admission control, concurrent launch, vsock exec/timeout/exit-code, snapshot restore latency, CoW page-sharing verification.

### Adversarial security scenarios (Linux + `/dev/kvm`, run as root)

```bash
sudo FC_KERNEL=/path/to/vmlinux FC_ROOTFS=/path/to/rootfs.ext4 \
  bash test/adversarial/run.sh
```

| Scenario | Payload | Pass condition |
|---|---|---|
| 01 — Fork bomb | `:(){ :|:& };:` | Host load bounded; `pids.max` enforced |
| 02 — Dir traversal | `cat /workspace/../../etc/shadow` | Output ≠ host `/etc/shadow` |
| 03 — Network exfil | `curl http://169.254.169.254/…` | Connection fails; no valid response |

**A failure in any scenario is a release blocker.**

---

## Project Structure

```
.
├── cmd/
│   └── controlplane/        # Binary entry point: HTTP server, graceful shutdown
├── internal/
│   ├── orchestrator/        # Firecracker lifecycle, cgroup v2, CID allocation, snapshot + pool
│   ├── gateway/             # REST API + WebSocket terminal proxy + bearer auth
│   ├── hitl/                # Redis-backed HITL classifier, approval queue, WaitForDecision
│   ├── protocol/            # Length-prefixed JSON wire format (host ↔ guest vsock)
│   ├── sandbox/             # Shared domain types (Record, Spec, Status)
│   └── state/               # Redis persistence, heartbeats, rate-limit token bucket
├── web/                     # React 18 + TypeScript + Tailwind + xterm.js dashboard
├── scripts/
│   ├── build-kernel.sh      # Build microVM vmlinux
│   ├── build-rootfs.sh      # Build Alpine ext4 rootfs with guest agent (Docker/apk)
│   ├── host-jailer-setup.sh # jaileruser, /srv paths, KVM ACL, binary capabilities
│   ├── host-kvm-setup.sh    # KVM group/ACL host prep
│   ├── build-seccomp.sh     # Compile seccomp-BPF filters via seccompiler-bin
│   ├── test-terminal.py     # WebSocket exec/HITL test client (no websocat needed)
│   └── vm-tap.sh            # Create/destroy per-VM TAP device for opt-in egress
├── deploy/
│   ├── firewall/sandbox.nft # nftables default-deny ruleset for sandbox subnet
│   ├── squid/squid.conf     # Forward proxy: dstdomain allowlist + IMDS block
│   ├── seccomp/             # vmm-filter.json (vmm / api / vcpu thread categories)
│   └── systemd/             # controlplane.service (Delegate=yes for cgroup subtree)
├── test/
│   ├── integration/         # vsock e2e: ping, exec, timeout, workspace I/O
│   ├── adversarial/         # 3 isolation scenarios + bash runner + common helpers
│   ├── security/            # Security regression tests (no KVM required)
│   ├── load/                # Concurrent boot, CID pool exhaustion, saturation point
│   └── bench/               # Snapshot restore latency, pool acquire, CoW page sharing
└── .github/
    └── workflows/ci.yml     # Unit → lint → security-regression → integration → adversarial
```

---

## License

MIT — see `LICENSE`.

---

*Built with Go 1.24 · Firecracker 1.x · Redis 7 · React 18 · xterm.js 5 · Vite 5 · Tailwind 3*
