package sys

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// BlockDevice represents a probed block storage device or partition.
type BlockDevice struct {
	Name      string // e.g. "sda", "sda1", "vda", "nvme0n1"
	Path      string // e.g. "/dev/sda"
	SizeBytes uint64
	SizeHuman string // e.g. "10.0G", "512.0M"
	Type      string // "disk" or "part"
	Mount     string // e.g. "/", "/boot" or "-"
	FSType    string // e.g. "ext4", "vfat" or "-"
	ReadOnly  bool
}

// BLKGETSIZE64 ioctl code: _IOR(0x12, 114, size_t)
const ioctlBlkGetSize64 = 0x80081272

// MountEntry represents a line from /proc/mounts.
type MountEntry struct {
	Device string
	Mount  string
	FSType string
}

// ParseMounts parses /proc/mounts (or an equivalent file) into a device-to-mount map.
func ParseMounts(mountsPath string) (map[string]MountEntry, error) {
	f, err := os.Open(mountsPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	mounts := make(map[string]MountEntry)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 3 {
			dev := fields[0]
			mounts[dev] = MountEntry{
				Device: dev,
				Mount:  fields[1],
				FSType: fields[2],
			}
		}
	}
	return mounts, sc.Err()
}

// FormatBytes formats a byte count into a human-readable string (e.g. 512.0M, 10.0G).
func FormatBytes(b uint64) string {
	const (
		_   = 1 << (10 * iota)
		kib
		mib
		gib
		tib
	)
	switch {
	case b >= tib:
		return fmt.Sprintf("%.1fT", float64(b)/float64(tib))
	case b >= gib:
		return fmt.Sprintf("%.1fG", float64(b)/float64(gib))
	case b >= mib:
		return fmt.Sprintf("%.1fM", float64(b)/float64(mib))
	case b >= kib:
		return fmt.Sprintf("%.1fK", float64(b)/float64(kib))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// ScanDisks probes the system for storage block devices (/dev/sd*, /dev/vd*, /dev/nvme*).
func ScanDisks() ([]BlockDevice, error) {
	return scanDisksFrom("/sys/class/block", "/proc/mounts", "/dev")
}

func scanDisksFrom(sysBlockDir, procMountsPath, devDir string) ([]BlockDevice, error) {
	mounts, _ := ParseMounts(procMountsPath)
	if mounts == nil {
		mounts = make(map[string]MountEntry)
	}

	found := make(map[string]BlockDevice)

	// First, check /sys/class/block entries if available.
	if entries, err := os.ReadDir(sysBlockDir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !isRelevantBlockDev(name) {
				continue
			}
			bd := readSysBlockDev(sysBlockDir, name, devDir, mounts)
			found[name] = bd
		}
	}

	// Also glob /dev for matching device nodes (handles systems where sysfs is partial)
	patterns := []string{
		filepath.Join(devDir, "sd*"),
		filepath.Join(devDir, "vd*"),
		filepath.Join(devDir, "nvme*"),
	}
	for _, pat := range patterns {
		matches, err := filepath.Glob(pat)
		if err != nil {
			continue
		}
		for _, devPath := range matches {
			name := filepath.Base(devPath)
			if !isRelevantBlockDev(name) {
				continue
			}
			if _, exists := found[name]; !exists {
				// Query directly via ioctl/stat
				bd := probeDevNode(devPath, name, mounts)
				found[name] = bd
			}
		}
	}

	result := make([]BlockDevice, 0, len(found))
	for _, bd := range found {
		result = append(result, bd)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result, nil
}

func isRelevantBlockDev(name string) bool {
	return strings.HasPrefix(name, "sd") ||
		strings.HasPrefix(name, "vd") ||
		strings.HasPrefix(name, "nvme")
}

func readSysBlockDev(sysBlockDir, name, devDir string, mounts map[string]MountEntry) BlockDevice {
	devPath := filepath.Join(devDir, name)
	sysPath := filepath.Join(sysBlockDir, name)

	var sizeBytes uint64
	if sizeData, err := os.ReadFile(filepath.Join(sysPath, "size")); err == nil {
		if sectors, err := strconv.ParseUint(strings.TrimSpace(string(sizeData)), 10, 64); err == nil {
			sizeBytes = sectors * 512
		}
	}

	isPart := false
	if _, err := os.Stat(filepath.Join(sysPath, "partition")); err == nil {
		isPart = true
	} else if isPartitionName(name) {
		isPart = true
	}

	readOnly := false
	if roData, err := os.ReadFile(filepath.Join(sysPath, "ro")); err == nil {
		readOnly = strings.TrimSpace(string(roData)) == "1"
	}

	devType := "disk"
	if isPart {
		devType = "part"
	}

	mountPoint := "-"
	fsType := "-"
	if m, ok := mounts[devPath]; ok {
		mountPoint = m.Mount
		fsType = m.FSType
	}

	return BlockDevice{
		Name:      name,
		Path:      devPath,
		SizeBytes: sizeBytes,
		SizeHuman: FormatBytes(sizeBytes),
		Type:      devType,
		Mount:     mountPoint,
		FSType:    fsType,
		ReadOnly:  readOnly,
	}
}

func probeDevNode(devPath, name string, mounts map[string]MountEntry) BlockDevice {
	var sizeBytes uint64
	f, err := os.Open(devPath)
	if err == nil {
		var sz uint64
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(ioctlBlkGetSize64), uintptr(unsafe.Pointer(&sz))); errno == 0 {
			sizeBytes = sz
		}
		f.Close()
	}

	isPart := isPartitionName(name)
	devType := "disk"
	if isPart {
		devType = "part"
	}

	mountPoint := "-"
	fsType := "-"
	if m, ok := mounts[devPath]; ok {
		mountPoint = m.Mount
		fsType = m.FSType
	}

	return BlockDevice{
		Name:      name,
		Path:      devPath,
		SizeBytes: sizeBytes,
		SizeHuman: FormatBytes(sizeBytes),
		Type:      devType,
		Mount:     mountPoint,
		FSType:    fsType,
	}
}

func isPartitionName(name string) bool {
	if strings.HasPrefix(name, "nvme") {
		return strings.Contains(name, "p")
	}
	if len(name) > 2 && (strings.HasPrefix(name, "sd") || strings.HasPrefix(name, "vd")) {
		lastChar := name[len(name)-1]
		return lastChar >= '0' && lastChar <= '9'
	}
	return false
}
