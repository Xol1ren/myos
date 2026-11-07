#!/bin/sh
# Usage: mkmodules.sh ROOTFS KVER MODULE...
# Resolves each module (and its dependencies, in load order) with the host's
# modprobe, decompresses it into ROOTFS/lib/modules and records the load
# order in ROOTFS/etc/modules for init. Built-in modules are skipped; modules
# that cannot be resolved are ignored.
root=$1
kver=$2
shift 2

mkdir -p "$root/lib/modules" "$root/etc"
: > "$root/etc/modules"

for m in "$@"; do
	modprobe -S "$kver" --show-depends "$m" 2>/dev/null | while read -r cmd path _; do
		[ "$cmd" = insmod ] || continue
		name=${path##*/}
		base=${name%%.ko*}
		dest="$root/lib/modules/$base.ko"
		if [ ! -f "$dest" ]; then
			case "$path" in
			*.xz) xz -dc "$path" ;;
			*.zst) zstd -dc "$path" ;;
			*.gz) gzip -dc "$path" ;;
			*) cat "$path" ;;
			esac >"$dest" || { rm -f "$dest"; continue; }
		fi
		grep -qx "/lib/modules/$base.ko" "$root/etc/modules" || echo "/lib/modules/$base.ko" >>"$root/etc/modules"
	done
done
echo "mkmodules: $(wc -l <"$root/etc/modules") module(s) staged"

