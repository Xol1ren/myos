// Package sys wraps the low-level Linux syscalls PID 1 needs (mounts,
// ioctl, module loading, shutdown) using only the standard library.
package sys

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Ioctl issues an ioctl(2) with a pointer argument.
func Ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

// Mount mounts a filesystem, treating "already mounted" (EBUSY) as success.
func Mount(source, target, fstype string, flags uintptr, data string) error {
	if err := syscall.Mount(source, target, fstype, flags, data); err != nil && err != syscall.EBUSY {
		return fmt.Errorf("mount %s on %s: %w", fstype, target, err)
	}
	return nil
}

// MountSpec describes one pseudo-filesystem mount.
type MountSpec struct {
	Source, Target, FSType string
	Flags                  uintptr
	Data                   string
}

// earlyMounts is the ordered boot-time mount table.
var earlyMounts = []MountSpec{
	{"devtmpfs", "/dev", "devtmpfs", 0, ""},
	{"proc", "/proc", "proc", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	{"sysfs", "/sys", "sysfs", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	{"tmpfs", "/tmp", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV, "mode=1777"},
}

// ValidateMounts checks a mount table: absolute, clean, unique targets, a
// filesystem type for each, and parents mounted before their children.
func ValidateMounts(specs []MountSpec) error {
	seen := map[string]int{}
	for i, m := range specs {
		switch {
		case m.FSType == "":
			return fmt.Errorf("mount %d (%s): empty filesystem type", i, m.Target)
		case !strings.HasPrefix(m.Target, "/") || m.Target == "/" || path.Clean(m.Target) != m.Target:
			return fmt.Errorf("mount %d: target %q must be an absolute, clean, non-root path", i, m.Target)
		}
		if _, dup := seen[m.Target]; dup {
			return fmt.Errorf("mount %d: duplicate target %s", i, m.Target)
		}
		seen[m.Target] = i
	}
	for i, m := range specs {
		for parent := path.Dir(m.Target); parent != "/"; parent = path.Dir(parent) {
			if j, ok := seen[parent]; ok && j > i {
				return fmt.Errorf("mount %s is ordered before its parent %s", m.Target, parent)
			}
		}
	}
	return nil
}

// MountEarly creates the base directories and mounts the pseudo filesystems.
// Individual failures are not fatal; the first error is returned.
func MountEarly() error {
	if err := ValidateMounts(earlyMounts); err != nil {
		return err
	}
	for _, d := range []string{"/dev", "/proc", "/sys", "/tmp", "/etc", "/root"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	var firstErr error
	for _, m := range earlyMounts {
		if err := Mount(m.Source, m.Target, m.FSType, m.Flags, m.Data); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	_ = os.Chmod("/tmp", 0o1777)
	return firstErr
}

// DefaultEnv are the variables exported when the environment lacks them.
var DefaultEnv = map[string]string{"TERM": "linux", "HOME": "/root", "PATH": "/bin:/sbin:/usr/bin:/usr/sbin"}

// SetDefaultEnv exports DefaultEnv entries that are unset or empty.
func SetDefaultEnv() {
	for k, v := range DefaultEnv {
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

// QuietKernel lowers the kernel console log level so printk does not
// scribble over the interactive shell.
func QuietKernel() {
	_ = os.WriteFile("/proc/sys/kernel/printk", []byte("3 4 1 3"), 0)
}

// ParseCmdline splits a kernel command line into key=value pairs. Bare
// flags map to "" and later duplicates win; double quotes group values.
func ParseCmdline(line string) map[string]string {
	out := map[string]string{}
	var fields []string
	var cur strings.Builder
	quoted := false
	for _, r := range line {
		switch {
		case r == '"':
			quoted = !quoted
		case (r == ' ' || r == '\t' || r == '\n') && !quoted:
			if cur.Len() > 0 {
				fields = append(fields, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		fields = append(fields, cur.String())
	}
	for _, f := range fields {
		k, v, _ := strings.Cut(f, "=")
		out[k] = v
	}
	return out
}

// CmdlineValue returns the value of key=value from /proc/cmdline.
func CmdlineValue(key string) string {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	return ParseCmdline(string(b))[key]
}

// AttachConsole opens path (default /dev/console) and makes it stdin,
// stdout and stderr.
func AttachConsole(path string) error {
	if path == "" {
		path = "/dev/console"
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	srcFd := int(f.Fd())
	var needClose bool
	if srcFd <= 2 {
		dupFd, err := syscall.Dup(srcFd)
		f.Close()
		if err != nil {
			return err
		}
		srcFd = dupFd
		needClose = true
	} else {
		defer f.Close()
	}
	for fd := 0; fd <= 2; fd++ {
		if srcFd != fd {
			if err := syscall.Dup2(srcFd, fd); err != nil {
				if needClose {
					syscall.Close(srcFd)
				}
				return err
			}
		}
	}
	if needClose {
		syscall.Close(srcFd)
	}
	os.Stdin = os.NewFile(0, "/dev/stdin")
	os.Stdout = os.NewFile(1, "/dev/stdout")
	os.Stderr = os.NewFile(2, "/dev/stderr")
	return nil
}

// ParseModuleList reads one module path per line (blank lines and '#'
// comments ignored), preserving order.
func ParseModuleList(r io.Reader) []string {
	var out []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if p := strings.TrimSpace(sc.Text()); p != "" && !strings.HasPrefix(p, "#") {
			out = append(out, p)
		}
	}
	return out
}

// loadAll inserts modules in list order. A module that fails is retried in
// later passes (a dependency listed after it may have been the cause) until a
// pass makes no progress; the remaining failures are returned. Already-loaded
// modules (EEXIST) count as success.
func loadAll(paths []string, load func(string) error) []error {
	pending := append([]string(nil), paths...)
	lastErr := map[string]error{}
	for len(pending) > 0 {
		var next []string
		for _, p := range pending {
			if err := load(p); err != nil && err != syscall.EEXIST {
				lastErr[p] = err
				next = append(next, p)
			}
		}
		if len(next) == len(pending) {
			break
		}
		pending = next
	}
	var errs []error
	for _, p := range pending {
		errs = append(errs, fmt.Errorf("%s: %w", p, lastErr[p]))
	}
	return errs
}

// LoadModules loads the kernel modules listed in listFile in order (the
// build step writes them in dependency order). Missing list is not an error.
func LoadModules(listFile string) []error {
	f, err := os.Open(listFile)
	if err != nil {
		return nil
	}
	defer f.Close()
	return loadAll(ParseModuleList(f), loadModule)
}

// readModuleImage returns the raw ELF image, gunzipping *.gz files.
func readModuleImage(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		if data, err = io.ReadAll(zr); err != nil {
			return nil, err
		}
	}
	if len(data) == 0 {
		return nil, syscall.EINVAL
	}
	return data, nil
}

func loadModule(path string) error {
	data, err := readModuleImage(path)
	if err != nil {
		return err
	}
	params := []byte{0}
	_, _, e := syscall.Syscall(syscall.SYS_INIT_MODULE,
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)), uintptr(unsafe.Pointer(&params[0])))
	if e != 0 {
		return e
	}
	return nil
}

// Mkdev encodes a device number the way glibc's makedev() does on Linux.
func Mkdev(major, minor uint32) uint64 {
	return uint64(major&0xfff)<<8 | uint64(minor&0xff) | uint64(minor&^0xff)<<12 | uint64(major&^0xfff)<<32
}

type devOps struct {
	exists func(string) bool
	mknod  func(path string, mode uint32, dev int) error
	sleep  func(time.Duration)
}

// ensureCharDevice waits up to timeout for path to appear (drivers register
// asynchronously) and creates the node itself if it never does. It reports
// whether it had to create the node.
func ensureCharDevice(path string, major, minor uint32, timeout time.Duration, o devOps) (bool, error) {
	const step = 100 * time.Millisecond
	for waited := time.Duration(0); ; waited += step {
		if o.exists(path) {
			return false, nil
		}
		if waited >= timeout {
			break
		}
		o.sleep(step)
	}
	if err := o.mknod(path, syscall.S_IFCHR|0o660, int(Mkdev(major, minor))); err != nil && err != syscall.EEXIST {
		return false, fmt.Errorf("mknod %s: %w", path, err)
	}
	return true, nil
}

// EnsureFramebuffer makes sure /dev/fb0 (char 29:0) exists, creating the node
// if devtmpfs did not. It returns true if it created the node. Note that a
// created node only opens successfully once a driver registered fb0.
func EnsureFramebuffer(timeout time.Duration) (bool, error) {
	return ensureCharDevice("/dev/fb0", 29, 0, timeout, devOps{
		exists: func(p string) bool { _, err := os.Stat(p); return err == nil },
		mknod:  func(p string, m uint32, d int) error { return syscall.Mknod(p, m, d) },
		sleep:  time.Sleep,
	})
}

// Shutdown stops all processes, syncs, and invokes reboot(2).
// cmd is syscall.LINUX_REBOOT_CMD_POWER_OFF or LINUX_REBOOT_CMD_RESTART.
// It refuses to run unless this process is PID 1 (kill(-1) is destructive).
func Shutdown(cmd int) error {
	if os.Getpid() != 1 {
		return fmt.Errorf("not running as PID 1; refusing to reboot the host")
	}
	_ = syscall.Kill(-1, syscall.SIGTERM)
	time.Sleep(300 * time.Millisecond)
	_ = syscall.Kill(-1, syscall.SIGKILL)
	syscall.Sync()
	_ = syscall.Unmount("/tmp", 0)
	syscall.Sync()
	return syscall.Reboot(cmd)
}
