# Sandboxed AI Agent Orchestration Platform

Hardware-isolated execution of untrusted AI-agent code using **AWS Firecracker
microVMs** (KVM), orchestrated by a Go control plane and driven by a React GUI.

Unlike containers, every sandbox runs against its **own guest kernel** behind a
KVM boundary, the VMM is **jailed** (chroot + PID namespace + uid/gid demotion),
host-side **cgroup v2** caps contain runaway guests, and host↔guest RPC travels
over **virtio-vsock** (no TCP/IP, no network attached by default).

> 📋 **Roadmap:** see [`planning/`](planning/) for the phase-by-phase plan
> (objectives, tasks, security notes, and Definition of Done per phase) and the
> [status dashboard](planning/README.md). Start with the
> [architecture & threat model](planning/00-overview.md).

## Security model (defense in depth)

| Layer | Control | Where |
|-------|---------|-------|
| Hypervisor | KVM microVM, separate guest kernel | Firecracker |
| Process | Jailer: chroot, PID ns, uid/gid demotion, seccomp-BPF | `internal/orchestrator` |
| Resources | cgroup v2 `cpu.max` / `memory.max` / `memory.swap.max` / `pids.max` | `internal/orchestrator` |
| Network | No NIC ("none"); opt-in TAP → Squid allowlist proxy | `deploy/squid` (Phase 5) |
| Transport | virtio-vsock, length-prefixed JSON on port 5005 | `internal/protocol` |
| In-guest | unprivileged uid 1000, process-group kill, timeout & output caps | `cmd/guest-agent` |
| Human gate | HITL approval for `write_file` / `execute_bash` | `internal/hitl` (Phase 4) |

## Repository layout

```
.
├── cmd/
│   ├── guest-agent/        guest_agent.go  — in-VM RPC daemon (vsock:5005)   [DONE]
│   └── controlplane/       main.go         — API gateway + WS proxy + HITL   [Phase 4]
├── internal/
│   ├── protocol/           length-prefixed JSON wire format (shared)         [DONE]
│   ├── orchestrator/       orchestrator.go — VM lifecycle, jailer, cgroups   [DONE]
│   ├── gateway/            WebSocket ↔ vsock bridge                          [Phase 4]
│   ├── hitl/               human-in-the-loop approval gate                   [Phase 4]
│   └── state/              Redis: heartbeats, session map, token buckets     [Phase 4]
├── web/                    React + Tailwind + xterm.js dashboard             [Phase 4]
├── deploy/
│   ├── kernel/             vmlinux build config                              [Phase 1]
│   ├── rootfs/             ext4 Alpine rootfs build scripts                  [Phase 1]
│   ├── seccomp/            seccomp-BPF allowlist (JSON → compiled)           [Phase 5]
│   ├── squid/              forward-proxy egress allowlist                    [Phase 5]
│   └── systemd/            control-plane unit (cgroup delegation)            [Phase 5]
├── scripts/                build / run helpers                               [Phase 1]
└── test/adversarial/       fork-bomb / traversal / exfiltration tests        [Phase 5]
```

## Status — delivered in this step

- `internal/protocol/protocol.go` — wire format + `protocol_test.go` (runs on any OS).
- `cmd/guest-agent/guest_agent.go` — production guest agent (`//go:build linux`).
- `internal/orchestrator/orchestrator.go` — jailer + cgroup v2 + CID engine (`//go:build linux`).

## Building

The agent and orchestrator are **Linux/KVM only** and are guarded with
`//go:build linux`, so they are skipped on Windows/macOS. The protocol package is
portable and can be tested anywhere:

```bash
go test ./internal/protocol/...                      # works on any OS, offline

# On a Linux KVM host, after `go mod tidy`:
make guest-agent                                     # static binary for the rootfs
go vet ./...
```

## SDK verification note

`internal/orchestrator/orchestrator.go` targets `firecracker-go-sdk v1.x`. Three
call sites are version-sensitive and flagged with `VERIFY-SDK` comments
(`NewNaiveChrootStrategy`, `JailerConfig.CgroupVersion`, `Machine.Shutdown`).
Reconcile them against the exact tag you vendor if compilation fails.

> Module path is `github.com/yourorg/sandbox-platform` — rename `yourorg` to your
> own and update the two internal import paths if you change it.
