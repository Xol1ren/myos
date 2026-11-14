package fb

import (
	"sync"
	"syscall"
	"time"
	"unsafe"

	"myos/internal/sys"
)

// KeyboardListener multiplexes keyboard input from both the primary console
// (typically fd 0 / serial console) and secondary virtual terminals
// (/dev/tty0, /dev/tty1 for QEMU graphical window focus) using Linux epoll
// and an eventfd shutdown descriptor so Close() wakes the poller immediately
// without leaking a reader goroutine or stealing bytes from fd 0.
type KeyboardListener struct {
	mu        sync.Mutex
	primaryFd int
	ttyFd     int
	epfd      int
	wakeFd    int
	ownedFds  []int
	restores  []func()
	keyChan   chan byte
	done      chan struct{}
	loopDone  chan struct{}
	closed    bool
}

// OpenDualKeyboard opens /dev/tty0 and /dev/tty1 (if available) alongside stdin (fd 0).
func OpenDualKeyboard() (*KeyboardListener, error) {
	return NewKeyboardListener(0, "/dev/tty0", "/dev/tty1")
}

// NewKeyboardListener initializes multiplexed keyboard input on primaryFd and secondaryPaths.
// It configures raw termios without echo, hides the hardware cursor via ANSI escape sequences,
// and starts a single epoll + eventfd multiplexer feeding into a buffered byte channel.
func NewKeyboardListener(primaryFd int, secondaryPaths ...string) (*KeyboardListener, error) {
	l := &KeyboardListener{
		primaryFd: primaryFd,
		ttyFd:     -1,
		epfd:      -1,
		wakeFd:    -1,
	}

	seenDevs := make(map[uint64]bool)

	// Primary console (typically serial or stdin fd 0)
	if primaryFd >= 0 {
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
	}

	// Secondary consoles (/dev/tty0, /dev/tty1) opened as raw FDs without os.NewFile GC finalizers
	for _, path := range secondaryPaths {
		if path == "" {
			continue
		}
		fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
		if err != nil {
			fd, err = syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
		}
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err == nil && st.Rdev != 0 {
			if seenDevs[st.Rdev] {
				// Duplicate of primary or previously opened device
				_ = syscall.Close(fd)
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
		l.ownedFds = append(l.ownedFds, fd)
	}

	l.ensureInit()
	return l, nil
}

func createEventFD() (int, error) {
	fd, _, errno := syscall.Syscall(syscall.SYS_EVENTFD2, 0, uintptr(syscall.O_CLOEXEC|syscall.O_NONBLOCK), 0)
	if errno != 0 {
		return -1, errno
	}
	return int(fd), nil
}

func (l *KeyboardListener) ensureInit() {
	if l.keyChan != nil {
		return
	}
	l.keyChan = make(chan byte, 512)
	l.done = make(chan struct{})
	l.loopDone = make(chan struct{})
	l.epfd = -1
	l.wakeFd = -1

	epfd, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		close(l.loopDone)
		return
	}
	wakeFd, err := createEventFD()
	if err != nil {
		_ = syscall.Close(epfd)
		close(l.loopDone)
		return
	}
	l.epfd = epfd
	l.wakeFd = wakeFd

	wakeEv := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(wakeFd)}
	_ = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, wakeFd, &wakeEv)

	addedFds := make(map[int]bool)
	activeInputs := 0
	addInput := func(fd int) {
		if fd < 0 || addedFds[fd] {
			return
		}
		ev := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(fd)}
		if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, fd, &ev); err == nil {
			addedFds[fd] = true
			activeInputs++
		}
	}

	addInput(l.primaryFd)
	addInput(l.ttyFd)
	for _, fd := range l.ownedFds {
		addInput(fd)
	}

	go l.epollLoop(epfd, wakeFd, activeInputs)
}

func (l *KeyboardListener) epollLoop(epfd, wakeFd, activeInputs int) {
	defer close(l.loopDone)
	var events [8]syscall.EpollEvent
	var buf [128]byte

	for {
		n, err := syscall.EpollWait(epfd, events[:], -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return
		}
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			if fd == wakeFd {
				return
			}
			m, rerr := syscall.Read(fd, buf[:])
			if m > 0 {
				for j := 0; j < m; j++ {
					select {
					case <-l.done:
						return
					case l.keyChan <- buf[j]:
					}
				}
			}
			if (m == 0 && rerr == nil) || (rerr != nil && rerr != syscall.EINTR && rerr != syscall.EAGAIN) {
				_ = syscall.EpollCtl(epfd, syscall.EPOLL_CTL_DEL, fd, nil)
				activeInputs--
				if activeInputs <= 0 {
					close(l.keyChan)
					return
				}
			}
		}
	}
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

// Close wakes the epoll multiplexer via eventfd, restores terminal states,
// and closes all owned descriptors without touching caller-owned primaryFd.
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
	wakeFd := l.wakeFd
	epfd := l.epfd
	loopDone := l.loopDone
	restores := l.restores
	l.restores = nil
	ownedFds := l.ownedFds
	l.ownedFds = nil
	l.wakeFd = -1
	l.epfd = -1
	l.ttyFd = -1
	l.mu.Unlock()

	if wakeFd >= 0 {
		var one = [8]byte{1, 0, 0, 0, 0, 0, 0, 0}
		_, _ = syscall.Write(wakeFd, one[:])
	}
	if loopDone != nil {
		<-loopDone
	}

	// Restore all termios and cursor modes
	for i := len(restores) - 1; i >= 0; i-- {
		if restores[i] != nil {
			restores[i]()
		}
	}

	if wakeFd >= 0 {
		_ = syscall.Close(wakeFd)
	}
	if epfd >= 0 {
		_ = syscall.Close(epfd)
	}

	var firstErr error
	for _, fd := range ownedFds {
		if fd >= 0 {
			if err := syscall.Close(fd); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}
