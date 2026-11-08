package fb

import (
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"myos/internal/sys"
)

// KeyboardListener multiplexes keyboard input from both the primary console
// (typically os.Stdin / serial console) and a secondary VT console (/dev/tty0
// for QEMU graphical window focus).
type KeyboardListener struct {
	mu         sync.Mutex
	primaryFd  int
	ttyFd      int
	restoreTTY func()
	restoreVT  func()
	closed     bool
}

// OpenDualKeyboard opens /dev/tty0 (or configured secondary console) alongside os.Stdin (fd 0).
func OpenDualKeyboard() (*KeyboardListener, error) {
	return NewKeyboardListener(int(os.Stdin.Fd()), "/dev/tty0")
}

// NewKeyboardListener initializes multiplexed keyboard input on primaryFd and secondaryPath.
// If secondaryPath cannot be opened or refers to the same device as primaryFd,
// it falls back to listening solely on primaryFd.
func NewKeyboardListener(primaryFd int, secondaryPath string) (*KeyboardListener, error) {
	l := &KeyboardListener{
		primaryFd: primaryFd,
		ttyFd:     -1,
	}

	// Ensure primary descriptor is in non-blocking mode so Read never hangs the GUI
	if primaryFd >= 0 {
		_ = syscall.SetNonblock(primaryFd, true)
	}

	if secondaryPath != "" {
		// Open secondary console in non-blocking mode
		fd, err := syscall.Open(secondaryPath, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			fd, err = syscall.Open(secondaryPath, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		}
		if err == nil && fd >= 0 {
			// Check if primary and secondary are the exact same device
			var stPrim, stSec syscall.Stat_t
			sameDevice := false
			if syscall.Fstat(primaryFd, &stPrim) == nil && syscall.Fstat(fd, &stSec) == nil {
				if stPrim.Rdev != 0 && stPrim.Rdev == stSec.Rdev {
					sameDevice = true
				}
			}
			if sameDevice {
				_ = syscall.Close(fd)
			} else {
				l.ttyFd = fd
				// Configure raw mode and graphics mode on secondary console
				l.restoreTTY = setRawMode(fd)
				l.restoreVT = GraphicsMode(fd)
			}
		}
	}

	return l, nil
}

// setRawMode puts fd into raw mode for character-at-a-time input without echo.
func setRawMode(fd int) func() {
	var old syscall.Termios
	if err := sys.Ioctl(fd, syscall.TCGETS, unsafe.Pointer(&old)); err != nil {
		return nil
	}
	raw := old
	raw.Iflag &^= syscall.ICRNL | syscall.INLCR | syscall.IGNCR | syscall.IXON | syscall.ISTRIP | syscall.BRKINT
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if err := sys.Ioctl(fd, syscall.TCSETS, unsafe.Pointer(&raw)); err != nil {
		return nil
	}
	return func() {
		_ = sys.Ioctl(fd, syscall.TCSETS, unsafe.Pointer(&old))
	}
}

// Poll waits up to timeout d for input from either input source using select.
// Returns read bytes, ok=true if data arrived, eof=true on EOF or unrecoverable error.
func (l *KeyboardListener) Poll(d time.Duration) (buf []byte, ok, eof bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, false, true
	}
	if d < 0 {
		d = 0
	}

	var fds syscall.FdSet
	maxFd := -1
	if l.primaryFd >= 0 && l.primaryFd < 1024 {
		fds.Bits[l.primaryFd/64] |= 1 << (uint(l.primaryFd) % 64)
		if l.primaryFd > maxFd {
			maxFd = l.primaryFd
		}
	}
	if l.ttyFd >= 0 && l.ttyFd < 1024 {
		fds.Bits[l.ttyFd/64] |= 1 << (uint(l.ttyFd) % 64)
		if l.ttyFd > maxFd {
			maxFd = l.ttyFd
		}
	}
	if maxFd < 0 {
		return nil, false, true
	}

	tv := syscall.NsecToTimeval(d.Nanoseconds())
	n, err := syscall.Select(maxFd+1, &fds, nil, nil, &tv)
	if err != nil || n <= 0 {
		return nil, false, false // timeout or EINTR
	}

	var scratch [128]byte
	var out []byte

	// Check primary console (os.Stdin / serial)
	if l.primaryFd >= 0 && l.primaryFd < 1024 && (fds.Bits[l.primaryFd/64]&(1<<(uint(l.primaryFd)%64))) != 0 {
		m, err := syscall.Read(l.primaryFd, scratch[:])
		if err == nil && m > 0 {
			out = append(out, scratch[:m]...)
		} else if m == 0 && err == nil {
			return nil, false, true // EOF on primary
		} else if err != nil && err != syscall.EINTR && err != syscall.EAGAIN {
			return nil, false, true
		}
	}

	// Check secondary console (/dev/tty0)
	if l.ttyFd >= 0 && l.ttyFd < 1024 && (fds.Bits[l.ttyFd/64]&(1<<(uint(l.ttyFd)%64))) != 0 {
		m, err := syscall.Read(l.ttyFd, scratch[:])
		if err == nil && m > 0 {
			out = append(out, scratch[:m]...)
		}
	}

	if len(out) > 0 {
		return out, true, false
	}
	return nil, false, false
}

// Close restores the terminal state and closes the secondary descriptor.
func (l *KeyboardListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true

	if l.primaryFd >= 0 {
		_ = syscall.SetNonblock(l.primaryFd, false)
	}

	if l.restoreVT != nil {
		l.restoreVT()
		l.restoreVT = nil
	}
	if l.restoreTTY != nil {
		l.restoreTTY()
		l.restoreTTY = nil
	}
	if l.ttyFd >= 0 {
		_ = syscall.Close(l.ttyFd)
		l.ttyFd = -1
	}
	return nil
}
