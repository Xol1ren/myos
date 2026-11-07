GO       ?= go
QEMU     ?= qemu-system-x86_64
KVER     ?= $(shell uname -r)
# Kernel image: override with KERNEL=/path/to/bzImage
KERNEL   ?= $(firstword $(wildcard /boot/vmlinuz-$(KVER) /usr/lib/modules/$(KVER)/vmlinuz /lib/modules/$(KVER)/vmlinuz /boot/vmlinuz))
# Kernel modules to stage into the initramfs (needed when the kernel builds
# drivers as modules, e.g. distro kernels). Use MODULES= to disable.
MODULES  ?= e1000 ehci_pci usbhid hid_generic mousedev evdev atkbd psmouse simpledrm bochs
# Set NOGRAPHIC=1 to run on the serial console in the current terminal.
NOGRAPHIC ?=
MEM      ?= 512M

BUILD     := build
ROOTFS    := $(BUILD)/rootfs
INIT_BIN  := $(ROOTFS)/init
INITRAMFS := $(BUILD)/initramfs.cpio.gz
SRCS      := $(shell find cmd internal -name '*.go') go.mod

ifeq ($(NOGRAPHIC),1)
  DISPLAY_ARGS := -nographic
  CMDLINE_TTY  := console=ttyS0 myos.tty=/dev/ttyS0
else
  # /dev/console follows the last console=; the shell is pinned to tty0
  # (keyboard + graphic window) via myos.tty, serial stays a log console.
  DISPLAY_ARGS := -serial stdio
  CMDLINE_TTY  := console=tty0 console=ttyS0 myos.tty=/dev/tty0
endif

ACCEL := $(if $(wildcard /dev/kvm),$(if $(shell test -w /dev/kvm && echo y),-enable-kvm -cpu host,),)

.PHONY: all build run iso run-iso run-iso-uefi vet test test-race clean

all: build

vet:
	$(GO) vet ./...

test:
	CGO_ENABLED=0 $(GO) test ./...

test-race:
	$(GO) test -race ./...

build: $(INITRAMFS)

$(INIT_BIN): $(SRCS)
	@mkdir -p $(ROOTFS)/dev $(ROOTFS)/proc $(ROOTFS)/sys $(ROOTFS)/tmp $(ROOTFS)/etc
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="-s -w" -o $(INIT_BIN) ./cmd/init

$(INITRAMFS): $(INIT_BIN) scripts/mkmodules.sh Makefile
	@rm -rf $(ROOTFS)/lib $(ROOTFS)/etc/modules
	@if [ -n "$(strip $(MODULES))" ]; then sh scripts/mkmodules.sh $(ROOTFS) $(KVER) $(MODULES); fi
	cd $(ROOTFS) && find . | cpio -o -H newc --owner=0:0 --quiet | gzip -9 > ../initramfs.cpio.gz
	@ls -lh $(INITRAMFS)

run: build
	@test -n "$(KERNEL)" -a -r "$(KERNEL)" || { echo "error: no readable kernel found; pass KERNEL=/path/to/vmlinuz" >&2; exit 1; }
	$(QEMU) $(ACCEL) -m $(MEM) -smp 2 \
		-kernel $(KERNEL) -initrd $(INITRAMFS) \
		-append "$(CMDLINE_TTY) quiet loglevel=3 rdinit=/init" \
		-net nic,model=e1000 -net user \
		-device usb-ehci -device usb-tablet \
		$(DISPLAY_ARGS)

iso: build
	KERNEL="$(KERNEL)" INITRAMFS="$(abspath $(INITRAMFS))" BUILD="$(abspath $(BUILD))" sh scripts/mkiso.sh

# Boot the ISO via legacy BIOS from the virtual CD-ROM.
run-iso: iso
	$(QEMU) $(ACCEL) -cdrom $(BUILD)/myos.iso -boot d -m $(MEM) \
		-net nic,model=e1000 -net user -device usb-ehci -device usb-tablet $(DISPLAY_ARGS)

# Same, via UEFI firmware (needs OVMF; override with OVMF=/path/OVMF_CODE.fd).
OVMF ?= $(firstword $(wildcard /usr/share/edk2/ovmf/OVMF_CODE.fd /usr/share/OVMF/OVMF_CODE.fd /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/ovmf/OVMF.fd))
run-iso-uefi: iso
	@test -n "$(OVMF)" || { echo "error: OVMF firmware not found; pass OVMF=..." >&2; exit 1; }
	$(QEMU) $(ACCEL) -drive if=pflash,format=raw,readonly=on,file=$(OVMF) \
		-cdrom $(BUILD)/myos.iso -boot d -m $(MEM) \
		-net nic,model=e1000 -net user -device usb-ehci -device usb-tablet $(DISPLAY_ARGS)

clean:
	rm -rf $(BUILD)

