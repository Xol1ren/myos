package sys

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMountWithExplicitFS(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mnt", "data")
	var gotDev, gotTarget, gotFS string
	err := mountWith("/dev/vda", dir, "ext4", func(source, target, fstype string, flags uintptr, data string) error {
		gotDev, gotTarget, gotFS = source, target, fstype
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatalf("target dir not created: %v", statErr)
	}
	if gotDev != "/dev/vda" || gotTarget != dir || gotFS != "ext4" {
		t.Fatalf("unexpected mount args: %q %q %q", gotDev, gotTarget, gotFS)
	}
}

func TestMountWithProbeFallback(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mnt", "probe")
	var tried []string
	err := mountWith("/dev/vda", dir, "", func(source, target, fstype string, flags uintptr, data string) error {
		tried = append(tried, fstype)
		if fstype == "iso9660" {
			return nil
		}
		return errors.New("wrong fs")
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tried) != 3 || tried[0] != "ext4" || tried[1] != "vfat" || tried[2] != "iso9660" {
		t.Fatalf("unexpected probe order: %v", tried)
	}
}

func TestMountWithAllFail(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mnt", "fail")
	err := mountWith("/dev/vda", dir, "", func(source, target, fstype string, flags uintptr, data string) error {
		return errors.New("no such device")
	})
	if err == nil {
		t.Fatal("expected error when all filesystems fail")
	}
}
