package sys

import (
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestEarlyMountTableIsValid(t *testing.T) {
	if err := ValidateMounts(earlyMounts); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"/dev": "devtmpfs", "/proc": "proc", "/sys": "sysfs"}
	for _, m := range earlyMounts {
		if fs, ok := want[m.Target]; ok && fs != m.FSType {
			t.Errorf("%s: fstype %s, want %s", m.Target, m.FSType, fs)
		}
		delete(want, m.Target)
	}
	if len(want) != 0 {
		t.Errorf("missing mounts: %v", want)
	}
}

func TestValidateMountsRejects(t *testing.T) {
	cases := map[string][]MountSpec{
		"empty fstype": {{"x", "/a", "", 0, ""}},
		"relative":     {{"x", "a", "tmpfs", 0, ""}},
		"root":         {{"x", "/", "tmpfs", 0, ""}},
		"unclean":      {{"x", "/a/../b", "tmpfs", 0, ""}},
		"duplicate":    {{"x", "/a", "tmpfs", 0, ""}, {"y", "/a", "tmpfs", 0, ""}},
		"child<parent": {{"x", "/dev/pts", "devpts", 0, ""}, {"y", "/dev", "devtmpfs", 0, ""}},
	}
	for name, specs := range cases {
		if ValidateMounts(specs) == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	ok := []MountSpec{{"y", "/dev", "devtmpfs", 0, ""}, {"x", "/dev/pts", "devpts", 0, ""}}
	if err := ValidateMounts(ok); err != nil {
		t.Errorf("valid table rejected: %v", err)
	}
}

func TestParseCmdline(t *testing.T) {
	m := ParseCmdline("quiet loglevel=3 myos.tty=/dev/tty0 foo=\"a b\"  video=1024x768\n")
	for k, v := range map[string]string{"quiet": "", "loglevel": "3", "myos.tty": "/dev/tty0", "foo": "a b", "video": "1024x768"} {
		if got, ok := m[k]; !ok || got != v {
			t.Errorf("%s = %q (present=%v), want %q", k, got, ok, v)
		}
	}
	if len(ParseCmdline("")) != 0 {
		t.Error("empty cmdline should parse to nothing")
	}
}

func TestParseModuleList(t *testing.T) {
	got := ParseModuleList(strings.NewReader("# c\n\n /lib/modules/a.ko \n/lib/modules/b.ko\n"))
	if len(got) != 2 || got[0] != "/lib/modules/a.ko" || got[1] != "/lib/modules/b.ko" {
		t.Fatalf("%q", got)
	}
}

func TestLoadAllOrderRetryAndEEXIST(t *testing.T) {
	// "b" needs "a"; listed first, it fails until a pass later.
	loaded := map[string]bool{}
	var order []string
	load := func(p string) error {
		switch p {
		case "b":
			if !loaded["a"] {
				return syscall.ENOENT
			}
		case "dup":
			return syscall.EEXIST
		case "bad":
			return syscall.EINVAL
		}
		loaded[p] = true
		order = append(order, p)
		return nil
	}
	errs := loadAll([]string{"b", "a", "dup", "bad"}, load)
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("order=%v", order)
	}
	if len(errs) != 1 || !errors.Is(errs[0], syscall.EINVAL) || !strings.Contains(errs[0].Error(), "bad") {
		t.Fatalf("errs=%v", errs)
	}
}

func TestLoadAllTerminatesWhenNothingProgresses(t *testing.T) {
	calls := 0
	errs := loadAll([]string{"x", "y"}, func(string) error { calls++; return syscall.EINVAL })
	if len(errs) != 2 || calls != 2 {
		t.Fatalf("errs=%v calls=%d", errs, calls)
	}
}

func TestReadModuleImage(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "m.ko")
	os.WriteFile(plain, []byte("\x7fELFdata"), 0o644)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("\x7fELFgz"))
	zw.Close()
	gz := filepath.Join(dir, "m.ko.gz")
	os.WriteFile(gz, buf.Bytes(), 0o644)
	empty := filepath.Join(dir, "e.ko")
	os.WriteFile(empty, nil, 0o644)

	if b, err := readModuleImage(plain); err != nil || string(b) != "\x7fELFdata" {
		t.Errorf("plain: %q %v", b, err)
	}
	if b, err := readModuleImage(gz); err != nil || string(b) != "\x7fELFgz" {
		t.Errorf("gz: %q %v", b, err)
	}
	if _, err := readModuleImage(empty); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("empty: %v", err)
	}
	if _, err := readModuleImage(filepath.Join(dir, "nope.ko")); err == nil {
		t.Error("missing file must fail")
	}
	if errs := LoadModules(filepath.Join(dir, "no-list")); errs != nil {
		t.Errorf("missing list is not an error: %v", errs)
	}
}

func TestMkdev(t *testing.T) {
	for _, c := range []struct {
		maj, min uint32
		want     uint64
	}{{29, 0, 0x1d00}, {8, 1, 0x801}, {1, 3, 0x103}, {259, 0, 0x10300}, {4, 300, 0x10042c}} {
		if got := Mkdev(c.maj, c.min); got != c.want {
			t.Errorf("Mkdev(%d,%d)=%#x want %#x", c.maj, c.min, got, c.want)
		}
	}
}

func TestEnsureCharDevice(t *testing.T) {
	var slept time.Duration
	var gotPath string
	var gotMode uint32
	var gotDev int
	ops := func(existsAfter int) devOps {
		n := 0
		return devOps{
			exists: func(string) bool { n++; return existsAfter >= 0 && n > existsAfter },
			mknod:  func(p string, m uint32, d int) error { gotPath, gotMode, gotDev = p, m, d; return nil },
			sleep:  func(d time.Duration) { slept += d },
		}
	}
	// appears by itself after 2 polls: no mknod
	gotPath = ""
	if created, err := ensureCharDevice("/dev/fb0", 29, 0, time.Second, ops(2)); err != nil || created || gotPath != "" {
		t.Fatalf("created=%v err=%v path=%q", created, err, gotPath)
	}
	// never appears: node created with 29:0, 0660 char device
	slept = 0
	created, err := ensureCharDevice("/dev/fb0", 29, 0, 300*time.Millisecond, ops(-1))
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if gotPath != "/dev/fb0" || gotMode != syscall.S_IFCHR|0o660 || gotDev != 29<<8 {
		t.Fatalf("mknod(%q, %#o, %#x)", gotPath, gotMode, gotDev)
	}
	if slept != 300*time.Millisecond {
		t.Fatalf("slept %v", slept)
	}
	// mknod failure is reported, EEXIST (race with devtmpfs) is not
	bad := devOps{exists: func(string) bool { return false }, sleep: func(time.Duration) {},
		mknod: func(string, uint32, int) error { return syscall.EPERM }}
	if _, err := ensureCharDevice("/dev/fb0", 29, 0, 0, bad); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("err=%v", err)
	}
	bad.mknod = func(string, uint32, int) error { return syscall.EEXIST }
	if _, err := ensureCharDevice("/dev/fb0", 29, 0, 0, bad); err != nil {
		t.Fatalf("EEXIST must be tolerated: %v", err)
	}
}

func TestSetDefaultEnv(t *testing.T) {
	t.Setenv("TERM", "")
	t.Setenv("HOME", "/custom")
	t.Setenv("PATH", "")
	SetDefaultEnv()
	if os.Getenv("TERM") != "linux" || os.Getenv("HOME") != "/custom" || !strings.HasPrefix(os.Getenv("PATH"), "/bin") {
		t.Fatalf("TERM=%q HOME=%q PATH=%q", os.Getenv("TERM"), os.Getenv("HOME"), os.Getenv("PATH"))
	}
}

func TestShutdownRefusesOutsidePID1(t *testing.T) {
	if os.Getpid() == 1 {
		t.Skip("running as PID 1")
	}
	if err := Shutdown(syscall.LINUX_REBOOT_CMD_POWER_OFF); err == nil {
		t.Fatal("Shutdown must refuse when not PID 1")
	}
}

func TestAttachConsole(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "console")
	if err := os.WriteFile(tmp, []byte("hello\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	s0, err0 := syscall.Dup(0)
	s1, err1 := syscall.Dup(1)
	s2, err2 := syscall.Dup(2)
	if err0 != nil || err1 != nil || err2 != nil {
		t.Skip("dup failed, skipping AttachConsole test")
	}
	defer func() {
		_ = syscall.Dup2(s0, 0)
		_ = syscall.Dup2(s1, 1)
		_ = syscall.Dup2(s2, 2)
		_ = syscall.Close(s0)
		_ = syscall.Close(s1)
		_ = syscall.Close(s2)
		os.Stdin = os.NewFile(0, "/dev/stdin")
		os.Stdout = os.NewFile(1, "/dev/stdout")
		os.Stderr = os.NewFile(2, "/dev/stderr")
	}()

	if err := AttachConsole(tmp); err != nil {
		t.Fatalf("AttachConsole failed: %v", err)
	}
	var b [5]byte
	if n, err := os.Stdin.Read(b[:]); err != nil || string(b[:n]) != "hello" {
		t.Fatalf("read from attached console: %q, %v", string(b[:n]), err)
	}
}
