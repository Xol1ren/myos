#!/bin/sh
# Build a hybrid (BIOS + UEFI, USB-bootable) ISO of myos.
#
#   scripts/mkiso.sh            (env overrides: KERNEL, INITRAMFS, OUT, BUILD)
#
# BIOS : ISOLINUX El Torito image + isohybrid MBR (dd/Rufus/Ventoy friendly)
# UEFI : GRUB EFI binary inside a FAT El Torito image + hybrid GPT
# If ISOLINUX is not installed but grub-mkrescue is, grub-mkrescue is used.
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
BUILD=${BUILD:-$ROOT/build}
INITRAMFS=${INITRAMFS:-$BUILD/initramfs.cpio.gz}
OUT=${OUT:-$BUILD/myos.iso}
STAGE=$BUILD/iso
KERNEL=${KERNEL:-}
CMDLINE=${CMDLINE:-"quiet loglevel=3 console=tty0 console=ttyS0,115200 mousedev.xres=1280 mousedev.yres=800"}
VOLID=MYOS

die() { echo "mkiso: error: $*" >&2; exit 1; }
warn() { echo "mkiso: warning: $*" >&2; }
have() { command -v "$1" >/dev/null 2>&1; }
first_tool() { for t in "$@"; do if have "$t"; then echo "$t"; return 0; fi; done; return 1; }
# first_file NAME DIR... -> prints the first existing DIR/NAME
first_file() {
	n=$1
	shift
	for d in "$@"; do
		if [ -f "$d/$n" ]; then echo "$d/$n"; return 0; fi
	done
	return 1
}

# --- dependencies -----------------------------------------------------------
have xorriso || die "xorriso not found (install xorriso)"
[ -n "$KERNEL" ] || KERNEL=$(first_file "vmlinuz-$(uname -r)" /boot) \
	|| KERNEL=$(first_file vmlinuz "/usr/lib/modules/$(uname -r)" "/lib/modules/$(uname -r)" /boot) \
	|| die "no kernel found; set KERNEL=/path/to/vmlinuz"
[ -r "$KERNEL" ] || die "kernel not readable: $KERNEL"
[ -r "$INITRAMFS" ] || die "initramfs missing: $INITRAMFS (run 'make build')"

SYSLINUX_DIRS="/usr/share/syslinux /usr/lib/syslinux /usr/lib/syslinux/bios /usr/lib/ISOLINUX /usr/share/syslinux/bios /usr/lib/syslinux/modules/bios"
ISOLINUX_BIN=$(first_file isolinux.bin $SYSLINUX_DIRS) || ISOLINUX_BIN=
LDLINUX=$(first_file ldlinux.c32 $SYSLINUX_DIRS) || LDLINUX=
ISOHDPFX=$(first_file isohdpfx.bin $SYSLINUX_DIRS /usr/lib/ISOLINUX) || ISOHDPFX=
GRUB_MKRESCUE=$(first_tool grub-mkrescue grub2-mkrescue) || GRUB_MKRESCUE=
GRUB_MKIMAGE=$(first_tool grub-mkimage grub2-mkimage) || GRUB_MKIMAGE=
GRUB_EFI_DIR=
for d in /usr/lib/grub/x86_64-efi /usr/lib64/grub/x86_64-efi /usr/share/grub/x86_64-efi; do
	[ -f "$d/normal.mod" ] && { GRUB_EFI_DIR=$d; break; }
done

USE_ISOLINUX=1
if [ -z "$ISOLINUX_BIN" ] || [ -z "$LDLINUX" ] || [ -z "$ISOHDPFX" ]; then
	[ -n "$GRUB_MKRESCUE" ] || die "need syslinux (isolinux.bin, ldlinux.c32, isohdpfx.bin) or grub-mkrescue"
	warn "ISOLINUX files not found; falling back to $GRUB_MKRESCUE"
	USE_ISOLINUX=0
fi

# --- staging tree -----------------------------------------------------------
rm -rf "$STAGE"
mkdir -p "$STAGE/boot/isolinux" "$STAGE/boot/grub" "$STAGE/EFI/BOOT"
cp "$KERNEL" "$STAGE/boot/vmlinuz"
cp "$INITRAMFS" "$STAGE/boot/initramfs.cpio.gz"
: >"$STAGE/boot/myos.marker"

cat >"$STAGE/boot/grub/grub.cfg" <<EOF
serial --speed=115200 --unit=0 --word=8 --parity=no --stop=1
terminal_input --append serial
terminal_output --append serial

set timeout=1
set default=0

menuentry "myos" {
	linux /boot/vmlinuz $CMDLINE
	initrd /boot/initramfs.cpio.gz
}

menuentry "myos (serial console ttyS0)" {
	linux /boot/vmlinuz quiet loglevel=3 console=ttyS0,115200 console=tty0 myos.tty=/dev/ttyS0 mousedev.xres=1280 mousedev.yres=800
	initrd /boot/initramfs.cpio.gz
}
EOF

if [ "$USE_ISOLINUX" = 0 ]; then
	"$GRUB_MKRESCUE" -o "$OUT" "$STAGE" -volid "$VOLID" >/dev/null 2>&1 || die "grub-mkrescue failed"
	echo "mkiso: built $OUT ($(du -h "$OUT" | cut -f1)) via grub-mkrescue"
	exit 0
fi

cp "$ISOLINUX_BIN" "$STAGE/boot/isolinux/isolinux.bin"
cp "$LDLINUX" "$STAGE/boot/isolinux/ldlinux.c32"
cat >"$STAGE/boot/isolinux/isolinux.cfg" <<EOF
SERIAL 0 115200
DEFAULT myos
PROMPT 1
TIMEOUT 10

LABEL myos
  KERNEL /boot/vmlinuz
  INITRD /boot/initramfs.cpio.gz
  APPEND $CMDLINE

LABEL serial
  KERNEL /boot/vmlinuz
  INITRD /boot/initramfs.cpio.gz
  APPEND quiet loglevel=3 console=ttyS0,115200 console=tty0 myos.tty=/dev/ttyS0 mousedev.xres=1280 mousedev.yres=800
EOF

# --- UEFI boot image (optional) -----------------------------------------------
EFI_ARGS=
if [ -n "$GRUB_MKIMAGE" ] && [ -n "$GRUB_EFI_DIR" ] && have mformat && have mmd && have mcopy; then
	EMBED=$BUILD/grub-embed.cfg
	cat >"$EMBED" <<'EOF'
search --no-floppy --file --set=root /boot/myos.marker
configfile ($root)/boot/grub/grub.cfg
EOF
	"$GRUB_MKIMAGE" -O x86_64-efi -d "$GRUB_EFI_DIR" -o "$BUILD/bootx64.efi" -p /boot/grub -c "$EMBED" \
		part_gpt part_msdos iso9660 fat normal linux configfile search search_fs_file echo test all_video gzio serial
	size=$(stat -c %s "$BUILD/bootx64.efi")
	rm -f "$STAGE/boot/efi.img"
	truncate -s "$(( (size / 1048576 + 3) ))M" "$STAGE/boot/efi.img"
	mformat -i "$STAGE/boot/efi.img" ::
	mmd -i "$STAGE/boot/efi.img" ::/EFI ::/EFI/BOOT
	mcopy -i "$STAGE/boot/efi.img" "$BUILD/bootx64.efi" ::/EFI/BOOT/BOOTX64.EFI
	EFI_ARGS="-eltorito-alt-boot -e boot/efi.img -no-emul-boot -isohybrid-gpt-basdat"
else
	warn "UEFI tooling missing (grub-mkimage + x86_64-efi modules + mtools); building BIOS-only ISO"
fi

# --- ISO ----------------------------------------------------------------------
# -isohybrid-mbr makes xorriso write the same hybrid MBR that the standalone
# `isohybrid` tool would, so the image can be dd'ed to USB sticks directly.
# (Running isohybrid again afterwards is unnecessary and can clobber the GPT.)
# shellcheck disable=SC2086
xorriso -as mkisofs -quiet -iso-level 3 -R -J -joliet-long -V "$VOLID" \
	-o "$OUT" \
	-isohybrid-mbr "$ISOHDPFX" \
	-c boot/isolinux/boot.cat \
	-b boot/isolinux/isolinux.bin -no-emul-boot -boot-load-size 4 -boot-info-table \
	$EFI_ARGS \
	"$STAGE"

echo "mkiso: built $OUT ($(du -h "$OUT" | cut -f1))"

