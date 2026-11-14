# myos: a pure-Go Linux userspace

[![CI Pipeline](https://github.com/Xol1ren/myos/actions/workflows/ci.yml/badge.svg)](https://github.com/Xol1ren/myos/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Static `CGO_ENABLED=0` Go binary as PID 1 (`/init`): no glibc, systemd or busybox.
It provides the init and reaper, DHCP networking with T1 renewal, an HTTP/HTTPS client,
block storage discovery, a PS/2 and tablet mouse tracker, an interactive shell with
job control and builtin coreutils, a TUI system monitor, and a 60 FPS DRM/KMS (or `/dev/fb0`)
graphical desktop with an embedded PTY terminal emulator.

## Layout

| Path | Purpose |
|------|---------|
| `cmd/init` | PID 1: mounts, console, modules, signals, network & DHCP renewal, mouse, shell |
| `internal/sys` | mount, ioctl, console TTY resolution (`TIOCSCTTY`), module loading, block disk scanner (`disks`), shutdown |
| `internal/proc` | atomic PID-registered zombie reaper (`wait4`) and child spawning (`Spawn`/`SpawnAttr`) |
| `internal/netcfg` | ioctl network setup, DHCPv4 client, and streaming HTTP/HTTPS client (`fetch`) |
| `internal/input` | `/dev/input/mice` PS/2 decoder, dynamic bounds clamping, and cursor state |
| `internal/shell` | `TIOCGWINSZ`-aware line editor, process-group job control (`TIOCSPGRP`), pipes, redirection, builtins |
| `internal/monitor` | `syshud` zero-allocation `/proc` & `sysinfo(2)` telemetry dashboard |
| `internal/fb` | DRM/KMS dumb-buffer & `/dev/fb0` backends, `epoll`+`eventfd` dual keyboard listener, PTY terminal (`/dev/ptmx`), 60 FPS desktop |
| `scripts/mkmodules.sh` | stage kernel modules into the initramfs |
| `scripts/mkiso.sh` | build the hybrid BIOS and UEFI ISO |

## Make targets

| Target | Description |
|--------|-------------|
| `make build` | Static `build/rootfs/init`, then `build/initramfs.cpio.gz` |
| `make run` | Boot with `-kernel`/`-initrd` in QEMU (`KERNEL=...`, `NOGRAPHIC=1` for serial) |
| `make iso` | Build `build/myos.iso` (ISOLINUX for BIOS, GRUB for UEFI, hybrid MBR and GPT) |
| `make run-iso` | Boot the ISO from a virtual CD-ROM, legacy BIOS |
| `make run-iso-uefi` | Boot the ISO through OVMF (`OVMF=/path/OVMF_CODE.fd`) |
| `make vet` / `make test` | `go vet ./...` / unit tests (monitor parsers, canvas, blit, cursor) |
| `make clean` | Remove `build/` |

Variables: `KERNEL`, `MODULES` (kernel modules to bundle; empty to disable), `MEM`, `QEMU`, `NOGRAPHIC`.

### ISO notes

- Dependencies: `xorriso` and syslinux (`isolinux.bin`, `ldlinux.c32`, `isohdpfx.bin`).
  For UEFI also `grub-mkimage` (or `grub2-mkimage`) with the `x86_64-efi` modules and `mtools`.
  Without the UEFI tools you get a BIOS-only ISO with a warning. Without syslinux,
  `grub-mkrescue` is used if present.
- The hybrid MBR is written by `xorriso -isohybrid-mbr`, so the image is ready to `dd` to a USB stick
  or to use with Rufus (DD mode) or Ventoy. Running `isohybrid` afterwards is not needed and can damage the GPT.
- The GRUB EFI binary is unsigned, so Secure Boot must be off.
- The kernel is taken from the host (`/boot/vmlinuz-$(uname -r)` or `/usr/lib/modules/.../vmlinuz`).
  Distro kernels need their drivers as modules, which `MODULES` bundles into the initramfs from the host's
  `/lib/modules`. The initramfs must be built against the same kernel version you boot.

## Shell builtins

`ls cat ip ping fetch disks mouse syshud gui cd pwd echo clear history help exit poweroff reboot`, plus:

- `syshud [iface]` shows live CPU and RAM bars, RX/TX KB/s (default `eth0`, falling back to the first
  non-loopback interface), process count (`sysinfo(2)`) and uptime. Each frame is written with one `write(2)` onto the
  alternate screen. Exit with `q` or Ctrl+C. The terminal, cursor and screen are always restored.
- `gui [device]` runs a 60 FPS graphical desktop using DRM/KMS dumb buffers (`/dev/dri/card*`, falling back to
  `/dev/fb0`): a top bar (clock, CPU, memory, backend indicator, and `[Exit]` button), a draggable system telemetry
  window, and an interactive PTY terminal emulator window (`/dev/ptmx`, `TIOCGPTPEER`, ANSI/UTF-8 grid).
  Keyboard input is multiplexed across `os.Stdin` and `/dev/tty0`–`/dev/tty1` via `epoll(7)` and `eventfd(2)` in
  `KD_TEXT` raw `termios` mode. Exit by clicking `[Exit]` or pressing `Ctrl+Q`.
- `fetch [-k] <url> [-o file]` downloads resources over HTTP/1.1 or HTTPS (TLS 1.2/1.3) with chunked transfer
  decoding, redirect following (up to 5 hops), bounded header parsing, and live progress telemetry.
- `disks` enumerates block devices and partitions (`/sys/class/block`, `/dev/sd*`, `/dev/vd*`, `/dev/nvme*`),
  reporting capacity (`BLKGETSIZE64` / sysfs sectors), device type, active mountpoint, and filesystem type from `/proc/mounts`.

### GUI implementation notes

- Display probing queries `/dev/dri/card*` via native Linux DRM/KMS ioctls (`DRM_IOCTL_MODE_GETRESOURCES`,
  `GETCONNECTOR`, `CREATE_DUMB`, `ADDFB`/`ADDFB2`, `MAP_DUMB`, `SETCRTC`, `DIRTYFB`) and falls back to legacy
  `/dev/fb0` (`FBIOGET_VSCREENINFO` / `FBIOGET_FSCREENINFO`, 16/24/32 bpp) when no DRM card is available.
- All drawing goes to an offscreen `[]uint32` canvas. `Blit` copies a full frame in one pass
  (a direct row copy for XRGB8888) and allocates nothing. This is verified by `testing.AllocsPerRun`.
- The embedded terminal allocates a master/slave PTY pair (`/dev/ptmx`, `TIOCSPTLCK`, `TIOCGPTPEER` with `TIOCGPTN`
  fallback, `TIOCSWINSZ`) and spawns `/init -shell` in a new session (`Setsid`, `Setctty`, `Ctty: 0`) via `proc.SpawnAttr`.
- The cursor uses boundary-clipped save-under, so the offscreen canvas never keeps cursor pixels.
- The font is the Linux console `default8x16` (ASCII 32-126), embedded in `internal/fb/font.go`.
- Under QEMU, the default `MODULES` list bundles `drm`, `drm_kms_helper`, `simpledrm`, `bochs`, `virtio-gpu`, and `mousedev`.

## Limitations

- `make run`, `make run-iso` (BIOS), and `make run-iso-uefi` (UEFI) have been verified under QEMU; DRM/KMS fallback diagnostics are provided when `/dev/fb0` is absent.
- ISO and QEMU targets require root privileges, host kernel images, and specialized toolchains (`xorriso`, `syslinux`, `mtools`, `qemu`), and are verified manually rather than in automated CI workflows.

