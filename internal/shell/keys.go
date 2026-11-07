package shell

import (
	"syscall"
	"time"
)

// pollKey waits up to d for one byte on stdin without blocking forever and
// without leaving a reader goroutine behind. eof is true if stdin hung up.
func pollKey(d time.Duration) (key byte, ok, eof bool) {
	return pollKeyFd(0, d)
}

func pollKeyFd(fd int, d time.Duration) (key byte, ok, eof bool) {
	if d < 0 {
		d = 0
	}
	if fd < 0 || fd >= 1024 {
		return 0, false, true
	}
	var fds syscall.FdSet
	fds.Bits[fd/64] |= 1 << (uint(fd) % 64)
	tv := syscall.NsecToTimeval(d.Nanoseconds())
	n, err := syscall.Select(fd+1, &fds, nil, nil, &tv)
	if err != nil || n <= 0 {
		return 0, false, false // timeout or EINTR
	}
	var b [1]byte
	m, err := syscall.Read(fd, b[:])
	if err != nil && err != syscall.EINTR && err != syscall.EAGAIN {
		return 0, false, true
	}
	if m <= 0 {
		return 0, false, err == nil // m==0 means EOF
	}
	return b[0], true, false
}

// waitQuit blocks for up to d and reports whether the user pressed a quit
// key: 'q', 'Q', Ctrl-C, or a lone Esc (Esc followed by more bytes is an
// arrow/function key escape sequence and is ignored). The terminal must be
// in raw mode.
func waitQuit(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		k, ok, eof := pollKey(time.Until(deadline))
		if eof {
			return true
		}
		if !ok {
			return false
		}
		switch k {
		case 'q', 'Q', 3:
			return true
		case 27:
			if _, more, _ := pollKey(25 * time.Millisecond); !more {
				return true
			}
			for { // swallow the rest of the escape sequence
				if _, more, _ := pollKey(5 * time.Millisecond); !more {
					break
				}
			}
		}
	}
}
