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
// (typically os.Stdin / serial console) and secondary virtual terminals
// (/dev/tty0, /dev/tty1 for QEMU graphical window focus).
// Dedicated reader goroutines block on read() and push incoming bytes to keyChan.
type KeyboardListener struct {
	mu        sync.Mutex
	primaryFd int
	ttyFd     int
	files     []*os.File
	restores  []func()
	keyChan   chan byte
	done      chan struct{}
	closed    bool
}

// OpenDualKeyboard opens /dev/tty0 and /dev/tty1 (if available) alongside os.Stdin (fd 0).
func OpenDualKeyboard() (*KeyboardListener, error) {
	return NewKeyboardListener(int(os.Stdin.Fd()), "/dev/tty0", "/dev/tty1")
}

// NewKeyboardListener initializes multiplexed keyboard input on primaryFd and secondaryPaths.
// It configures raw termios without echo, hides the hardware cursor via ANSI escape sequences,
// and spawns dedicated reader goroutines feeding into a buffered byte channel.
func NewKeyboardListener(primaryFd int, secondaryPaths ...string) (*KeyboardListener, error) {
	l := &KeyboardListener{
		primaryFd: primaryFd,
		ttyFd:     -1,
		keyChan:   make(chan byte, 512),
		done:      make(chan struct{}),
	}

	seenDevs := make(map[uint64]bool)

	// Primary console (typically serial or os.Stdin)
	if primaryFd >= 0 {
		_ = syscall.SetNonblock(primaryFd, false)
		var st syscall.Stat_t
		if err := syscall.Fstat(primaryFd, &st); err == nil && st.Rdev != 0 {
			seenDevs[st.Rdev] = true
		}
		if r := setRawMode(primaryFd); r != nil {
			l.restores = append(l.restores, r)
		}
		if r := hideCursor(primaryFd); r != nil {
			l.restores = append(l.restores, r)
		}
		var primFile *os.File
		if primaryFd == int(os.Stdin.Fd()) {
			primFile = os.Stdin
		} else {
			primFile = os.NewFile(uintptr(primaryFd), "primary")
		}
		l.startReader(primFile, true)
	}

	// Secondary consoles (/dev/tty0, /dev/tty1)
	for _, path := range secondaryPaths {
		if path == "" {
			continue
		}
		f, err := os.OpenFile(path, os.O_RDWR|syscall.O_CLOEXEC, 0)
		if err != nil {
			f, err = os.OpenFile(path, os.O_RDONLY|syscall.O_CLOEXEC, 0)
		}
		if err != nil {
			continue
		}
		fd := int(f.Fd())
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err == nil && st.Rdev != 0 {
			if seenDevs[st.Rdev] {
				// Duplicate of primary or previously opened device
				_ = f.Close()
				continue
			}
			seenDevs[st.Rdev] = true
		}

		if l.ttyFd < 0 {
			l.ttyFd = fd
		}
		if r := setRawMode(fd); r != nil {
			l.restores = append(l.restores, r)
		}
		if r := hideCursor(fd); r != nil {
			l.restores = append(l.restores, r)
		}
		l.files = append(l.files, f)
		l.startReader(f, false)
	}

	return l, nil
}

func (l *KeyboardListener) ensureInit() {
	if l.keyChan == nil {
		l.keyChan = make(chan byte, 512)
		l.done = make(chan struct{})
		if l.primaryFd >= 0 {
			_ = syscall.SetNonblock(l.primaryFd, false)
			var f *os.File
			if l.primaryFd == int(os.Stdin.Fd()) {
				f = os.Stdin
			} else {
				f = os.NewFile(uintptr(l.primaryFd), "primary")
			}
			l.startReader(f, true)
		}
		if l.ttyFd >= 0 {
			f := os.NewFile(uintptr(l.ttyFd), "secondary")
			l.startReader(f, false)
		}
	}
}

func (l *KeyboardListener) startReader(f *os.File, isPrimary bool) {
	if f == nil {
		return
	}
	go func() {
		buf := make([]byte, 128)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				for i := 0; i < n; i++ {
					select {
					case <-l.done:
						return
					case l.keyChan <- buf[i]:
					}
				}
			}
			if err != nil {
				return
			}
			select {
			case <-l.done:
				return
			default:
			}
		}
	}()
}

// setRawMode puts fd into raw mode for character-at-a-time input without echo.
// It disables ICANON, ECHO, ISIG, IEXTEN, and ICRNL, with VMIN=1, VTIME=0.
func setRawMode(fd int) func() {
	if fd < 0 {
		return nil
	}
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

// hideCursor writes ANSI \033[?25l to hide the blinking hardware cursor on fd
// and returns a restore function that writes \033[?25h.
func hideCursor(fd int) func() {
	if fd < 0 {
		return nil
	}
	_, _ = syscall.Write(fd, []byte("\x1b[?25l"))
	return func() {
		_, _ = syscall.Write(fd, []byte("\x1b[?25h"))
	}
}

// Poll waits up to timeout d for input from any monitored console.
// Returns read bytes, ok=true if data arrived, eof=true on EOF or unrecoverable error.
func (l *KeyboardListener) Poll(d time.Duration) (buf []byte, ok, eof bool) {
	l.mu.Lock()
	l.ensureInit()
	if l.closed {
		l.mu.Unlock()
		return nil, false, true
	}
	done := l.done
	keyChan := l.keyChan
	l.mu.Unlock()

	var first byte
	if d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-done:
			return nil, false, true
		case b, ok := <-keyChan:
			if !ok {
				return nil, false, true
			}
			first = b
		case <-timer.C:
			return nil, false, false
		}
	} else {
		select {
		case <-done:
			return nil, false, true
		case b, ok := <-keyChan:
			if !ok {
				return nil, false, true
			}
			first = b
		default:
			return nil, false, false
		}
	}

	out := []byte{first}
	// If first byte is ESC (0x1b), wait a brief moment for subsequent escape sequence bytes
	if first == 0x1b {
		escTimer := time.NewTimer(15 * time.Millisecond)
		select {
		case b, ok := <-keyChan:
			escTimer.Stop()
			if ok {
				out = append(out, b)
			}
		case <-escTimer.C:
		}
	}
drain:
	for {
		select {
		case b, ok := <-keyChan:
			if !ok {
				break drain
			}
			out = append(out, b)
		default:
			// If we have an ESC sequence started (e.g. \x1b[), allow a small window for the command byte
			if len(out) >= 2 && out[0] == 0x1b && len(out) < 8 {
				seqTimer := time.NewTimer(10 * time.Millisecond)
				select {
				case b, ok := <-keyChan:
					seqTimer.Stop()
					if ok {
						out = append(out, b)
						continue
					}
				case <-seqTimer.C:
				}
			}
			break drain
		}
	}

	return out, true, false
}

// Close restores terminal states and closes all opened secondary descriptors.
func (l *KeyboardListener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	if l.done != nil {
		close(l.done)
	}
	l.mu.Unlock()

	// Ensure primary descriptor remains strictly in blocking mode
	if l.primaryFd >= 0 {
		_ = syscall.SetNonblock(l.primaryFd, false)
	}

	// Restore all termios and cursor modes
	for i := len(l.restores) - 1; i >= 0; i-- {
		if l.restores[i] != nil {
			l.restores[i]()
		}
	}
	l.restores = nil

	// Close opened secondary files to unblock reader goroutines
	var firstErr error
	for _, f := range l.files {
		if f != nil {
			if err := f.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	l.files = nil
	l.ttyFd = -1

	return firstErr
}
