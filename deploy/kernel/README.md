# Guest kernel (`vmlinux`)

Firecracker boots a raw, uncompressed `vmlinux` (not `bzImage`) with no
initramfs, so every device the guest needs at boot must be compiled **built-in**
(`=y`), never as a module.

## How the config is produced

We do **not** vendor a full multi-thousand-line `.config` (it is large, version
specific, and drifts). Instead [`build-kernel.sh`](../../scripts/build-kernel.sh):

1. Downloads Firecracker's published microVM config for the kernel version
   (`KVER`, default `6.1`).
2. Appends [`required-config.fragment`](required-config.fragment) — our must-have
   built-in options.
3. Runs `make olddefconfig` to resolve everything else.
4. Asserts the must-haves with
   [`check-kernel-config.sh`](../../scripts/check-kernel-config.sh).
5. Saves the resolved config here as `microvm-kernel-x86_64-<KVER>.config` and
   builds `vmlinux-<KVER>`.

## Build

```bash
scripts/build-kernel.sh                 # default KVER=6.1
KVER=6.1 OUT=./vmlinux-6.1 scripts/build-kernel.sh
```

## Required built-in options (lint)

`CONFIG_VSOCKETS`, `CONFIG_VIRTIO_VSOCKETS`, `CONFIG_VIRTIO`,
`CONFIG_VIRTIO_MMIO`, `CONFIG_VIRTIO_BLK`, `CONFIG_EXT4_FS`,
`CONFIG_SERIAL_8250`, `CONFIG_SERIAL_8250_CONSOLE`.

Run the lint standalone (e.g. in CI) against any config:

```bash
scripts/check-kernel-config.sh deploy/kernel/microvm-kernel-x86_64-6.1.config
```

The built `vmlinux-*` and the resolved `*.config` are git-ignored (large /
generated). Point the control plane at the image with `KERNEL_IMAGE`.
