# myos: a pure-Go Linux userspace

[![CI Pipeline](https://github.com/Xol1ren/myos/actions/workflows/ci.yml/badge.svg)](https://github.com/Xol1ren/myos/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Static `CGO_ENABLED=0` Go binary as PID 1 (`/init`): no glibc, systemd or busybox.
It provides the init and reaper, DHCP networking, a PS/2 mouse tracker, a shell with
builtin coreutils, a TUI system monitor and a framebuffer desktop.

## Layout

| Path | Purpose |
|------|---------|
| `cmd/init` | PID 1: mounts, console, modules, signals, network, mouse, shell |
| `internal/sys` | mount, ioctl, module loading, shutdown |
| `internal/proc` | zombie reaper and child spawning |
| `internal/netcfg` | ioctl network setup and DHCP client |
| `internal/input` | `/dev/input/mice` decoder and cursor state |
| `internal/shell` | line editor, pipes and redirection, builtins |
| `internal/monitor` | `syshud` /proc telemetry dashboard |
| `internal/fb` | `/dev/fb0` mmap, canvas, font, cursor, demo desktop |
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

`ls cat ip ping mouse cd pwd echo clear history help exit poweroff reboot`, plus:

- `syshud [iface]` shows live CPU and RAM bars, RX/TX KB/s (default `eth0`, falling back to the first
  non-loopback interface), process count and uptime. Each frame is written with one `write(2)` onto the
  alternate screen. Exit with `q` or Ctrl+C. The terminal, cursor and screen are always restored.
- `gui [/dev/fbN]` runs a 60 FPS desktop on the framebuffer: a navy background, a top bar
  (clock, memory, IP), a draggable window and a click counter, all driven by the mouse tracker.
  Exit with `Esc` or `q`. A lone Esc quits; arrow-key escape sequences are ignored.
  The VT is put in `KD_GRAPHICS` mode while the GUI runs and restored on exit.
  Mouse bounds are set to the screen size for the duration and restored afterwards.

### GUI implementation notes

- The framebuffer is configured via `FBIOGET_VSCREENINFO` and `FBIOGET_FSCREENINFO`, then mapped with
  `mmap(MAP_SHARED)`. 16, 24 and 32 bpp are supported, using the driver's bit-field layout.
- All drawing goes to an offscreen `[]uint32` canvas. `Blit` copies a full frame in one pass
  (a direct row copy for XRGB8888) and allocates nothing. This is verified by `testing.AllocsPerRun`.
- The cursor uses save-under, so the offscreen canvas never keeps cursor pixels.
- The font is the Linux console `default8x16` (ASCII 32-126), embedded in `internal/fb/font.go`.
- You need a framebuffer device for `gui`. Under QEMU's default VGA the kernel needs `simpledrm`, `bochs`
  or `vesafb`; the default `MODULES` list includes `simpledrm` and `bochs`.

## Limitations

- DHCP leases are not renewed.
- There is no job control, so Ctrl-C cannot interrupt external programs.
- Line editing assumes the input fits on one terminal row.
- `make run`, `make run-iso` (BIOS), and `make run-iso-uefi` (UEFI) have been verified under QEMU; DRM/KMS fallback diagnostics are provided when `/dev/fb0` is absent.
- ISO and QEMU targets require root privileges, host kernel images, and specialized toolchains (`xorriso`, `syslinux`, `mtools`, `qemu`), and are verified manually rather than in automated CI workflows.

## Documentation

- [docs/SELF_REVIEW_RC3.md](docs/SELF_REVIEW_RC3.md): Release Candidate 3 self-review detailing PTY emulation, HTTP engine, storage scanner, test coverage, and benchmark profiles.

