package sys

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		bytes uint64
		want  string
	}{
		{500, "500B"},
		{1024, "1.0K"},
		{1024 * 1024 * 512, "512.0M"},
		{1024 * 1024 * 1024 * 10, "10.0G"},
		{1024 * 1024 * 1024 * 1024 * 2, "2.0T"},
	}
	for _, tc := range cases {
		got := FormatBytes(tc.bytes)
		if got != tc.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

func TestParseMounts(t *testing.T) {
	tmp := t.TempDir()
	mountsFile := filepath.Join(tmp, "mounts")
	data := `/dev/sda1 /boot ext4 rw,relatime 0 0
/dev/sda2 / ext4 rw,relatime 0 0
/dev/sdb1 /mnt/usb\040drive vfat rw 0 0
tmpfs /tmp tmpfs rw,nosuid,nodev 0 0
`
	if err := os.WriteFile(mountsFile, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	mounts, err := ParseMounts(mountsFile)
	if err != nil {
		t.Fatalf("ParseMounts failed: %v", err)
	}

	if m, ok := mounts["/dev/sda1"]; !ok || m.Mount != "/boot" || m.FSType != "ext4" {
		t.Errorf("expected /dev/sda1 mounted at /boot ext4, got %+v", m)
	}
	if m, ok := mounts["/dev/sda2"]; !ok || m.Mount != "/" || m.FSType != "ext4" {
		t.Errorf("expected /dev/sda2 mounted at / ext4, got %+v", m)
	}
	if m, ok := mounts["/dev/sdb1"]; !ok || m.Mount != "/mnt/usb drive" || m.FSType != "vfat" {
		t.Errorf("expected /dev/sdb1 mounted at '/mnt/usb drive' vfat, got %+v", m)
	}
}

func TestScanDisksMock(t *testing.T) {
	tmp := t.TempDir()
	sysBlock := filepath.Join(tmp, "sys", "class", "block")
	procMounts := filepath.Join(tmp, "proc", "mounts")
	devDir := filepath.Join(tmp, "dev")

	if err := os.MkdirAll(filepath.Join(sysBlock, "sda"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sysBlock, "sda1"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(procMounts), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(devDir, 0755); err != nil {
		t.Fatal(err)
	}

	// 10 GB disk = 20971520 sectors
	_ = os.WriteFile(filepath.Join(sysBlock, "sda", "size"), []byte("20971520\n"), 0644)
	_ = os.WriteFile(filepath.Join(sysBlock, "sda", "ro"), []byte("0\n"), 0644)

	// 512 MB part = 1048576 sectors
	_ = os.WriteFile(filepath.Join(sysBlock, "sda1", "size"), []byte("1048576\n"), 0644)
	_ = os.WriteFile(filepath.Join(sysBlock, "sda1", "partition"), []byte("1\n"), 0644)

	mountsContent := filepath.Join(devDir, "sda1") + " /boot ext4 rw 0 0\n"
	_ = os.WriteFile(procMounts, []byte(mountsContent), 0644)

	disks, err := scanDisksFrom(sysBlock, procMounts, devDir)
	if err != nil {
		t.Fatalf("scanDisksFrom failed: %v", err)
	}

	if len(disks) != 2 {
		t.Fatalf("expected 2 disks, got %d", len(disks))
	}

	d0 := disks[0]
	if d0.Name != "sda" || d0.Type != "disk" || d0.SizeHuman != "10.0G" || d0.Mount != "-" {
		t.Errorf("unexpected sda: %+v", d0)
	}

	d1 := disks[1]
	if d1.Name != "sda1" || d1.Type != "part" || d1.SizeHuman != "512.0M" || d1.Mount != "/boot" || d1.FSType != "ext4" {
		t.Errorf("unexpected sda1: %+v", d1)
	}
}
