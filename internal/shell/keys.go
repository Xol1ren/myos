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

// pollDualBytes waits up to d for input from either fd1 or fd2.
func pollDualBytes(fd1, fd2 int, d time.Duration) (buf []byte, ok, eof bool) {
	if d < 0 {
		d = 0
	}
	var fds syscall.FdSet
	maxFd := -1
	if fd1 >= 0 && fd1 < 1024 {
		fds.Bits[fd1/64] |= 1 << (uint(fd1) % 64)
		if fd1 > maxFd {
			maxFd = fd1
		}
	}
	if fd2 >= 0 && fd2 < 1024 {
		fds.Bits[fd2/64] |= 1 << (uint(fd2) % 64)
		if fd2 > maxFd {
			maxFd = fd2
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
	if fd1 >= 0 && fd1 < 1024 && (fds.Bits[fd1/64]&(1<<(uint(fd1)%64))) != 0 {
		m, err := syscall.Read(fd1, scratch[:])
		if err == nil && m > 0 {
			res := make([]byte, m)
			copy(res, scratch[:m])
			return res, true, false
		}
		if err != nil && err != syscall.EINTR && err != syscall.EAGAIN {
			return nil, false, true
		}
		if m == 0 && err == nil {
			return nil, false, true
		}
	}
	if fd2 >= 0 && fd2 < 1024 && (fds.Bits[fd2/64]&(1<<(uint(fd2)%64))) != 0 {
		m, err := syscall.Read(fd2, scratch[:])
		if err == nil && m > 0 {
			res := make([]byte, m)
			copy(res, scratch[:m])
			return res, true, false
		}
	}
	return nil, false, false
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
