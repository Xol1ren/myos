package sys

import (
	"fmt"
	"os"
	"syscall"
)

// defaultFSTypes is the probe order when Mount is called with an empty fstype.
var defaultFSTypes = []string{"ext4", "vfat", "iso9660", "tmpfs"}

// MountAuto mounts device onto target. If fstype is empty, common filesystems
// (ext4, vfat, iso9660, tmpfs) are probed in order. Target is created if absent.
func MountAuto(device, target, fstype string) error {
	return mountWith(device, target, fstype, syscall.Mount)
}

func mountWith(
	device, target, fstype string,
	mountFn func(source, target, fstype string, flags uintptr, data string) error,
) error {
	if device == "" || target == "" {
		return fmt.Errorf("device and target must be non-empty")
	}
	if err := os.MkdirAll(target, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", target, err)
	}
	if fstype != "" {
		return mountFn(device, target, fstype, 0, "")
	}
	var lastErr error
	for _, fs := range defaultFSTypes {
		if err := mountFn(device, target, fs, 0, ""); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no filesystem types to probe")
	}
	return fmt.Errorf("mount %s on %s failed: %w", device, target, lastErr)
}

// Umount unmounts the filesystem at target.
func Umount(target string) error {
	if target == "" {
		return fmt.Errorf("target must be non-empty")
	}
	return syscall.Unmount(target, 0)
}
