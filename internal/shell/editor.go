package shell

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unicode/utf8"
	"unsafe"

	"myos/internal/sys"
)

const ioctlTIOCGWINSZ = 0x5413

type ttyWinsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

func termCols(fd int) int {
	if fd >= 0 {
		var ws ttyWinsize
		if err := sys.Ioctl(fd, ioctlTIOCGWINSZ, unsafe.Pointer(&ws)); err == nil && ws.Col > 0 {
			return int(ws.Col)
		}
	}
	return 80
}

func isTTY(fd int) bool {
	var t syscall.Termios
	return sys.Ioctl(fd, syscall.TCGETS, unsafe.Pointer(&t)) == nil
}

// makeRaw puts fd into raw mode (output post-processing stays enabled so
// "\n" still renders as CR+LF) and returns a restore function.
func makeRaw(fd int) (func(), error) {
	var old syscall.Termios
	if err := sys.Ioctl(fd, syscall.TCGETS, unsafe.Pointer(&old)); err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.ICRNL | syscall.INLCR | syscall.IGNCR | syscall.IXON | syscall.ISTRIP | syscall.BRKINT
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if err := sys.Ioctl(fd, syscall.TCSETS, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	return func() { _ = sys.Ioctl(fd, syscall.TCSETS, unsafe.Pointer(&old)) }, nil
}

// readLine reads one line with editing and history. It returns io.EOF-like
// os.ErrClosed semantics via the eof flag when Ctrl-D is pressed on an empty line.
func (s *Shell) readLine(prompt string) (line string, eof bool) {
	if !s.tty {
		l, err := s.stdin.ReadString('\n')
		if err != nil && l == "" {
			return "", true
		}
		return strings.TrimRight(l, "\r\n"), false
	}
	out := s.outFile()
	inFd := int(s.inFile().Fd())
	restore, err := makeRaw(inFd)
	if err != nil {
		l, err := s.stdin.ReadString('\n')
		if err != nil && l == "" {
			return "", true
		}
		return strings.TrimRight(l, "\r\n"), false
	}
	defer restore()

	var buf []rune
	pos := 0
	histIdx := len(s.history)
	saved := ""

	redraw := func() {
		cols := termCols(inFd)
		promptLen := utf8.RuneCountInString(prompt)
		maxEdit := cols - promptLen - 1
		vis := buf
		curCol := promptLen + pos
		if maxEdit > 0 && len(buf) > maxEdit {
			start := 0
			if pos >= maxEdit {
				start = pos - maxEdit + 1
			}
			end := min(start+maxEdit, len(buf))
			vis = buf[start:end]
			curCol = promptLen + (pos - start)
		}
		var sb strings.Builder
		sb.WriteString("\r" + prompt + string(vis) + "\x1b[K\r")
		if curCol > 0 {
			fmt.Fprintf(&sb, "\x1b[%dC", curCol)
		}
		_, _ = out.WriteString(sb.String())
	}
	setLine := func(t string) {
		buf = []rune(t)
		pos = len(buf)
	}
	redraw()

	for {
		r, _, err := s.stdin.ReadRune()
		if err != nil {
			if errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EAGAIN) {
				continue
			}
			return "", true
		}
		switch r {
		case '\r', '\n':
			_, _ = out.WriteString("\r\n")
			return string(buf), false
		case 3: // Ctrl-C
			_, _ = out.WriteString("^C\r\n")
			return "", false
		case 4: // Ctrl-D
			if len(buf) == 0 {
				_, _ = out.WriteString("\r\n")
				return "", true
			}
			if pos < len(buf) {
				buf = append(buf[:pos], buf[pos+1:]...)
			}
		case 127, 8: // backspace
			if pos > 0 {
				buf = append(buf[:pos-1], buf[pos:]...)
				pos--
			}
		case 1: // Ctrl-A
			pos = 0
		case 5: // Ctrl-E
			pos = len(buf)
		case 2: // Ctrl-B
			if pos > 0 {
				pos--
			}
		case 6: // Ctrl-F
			if pos < len(buf) {
				pos++
			}
		case 11: // Ctrl-K
			buf = buf[:pos]
		case 21: // Ctrl-U
			buf = buf[pos:]
			pos = 0
		case 23: // Ctrl-W: delete previous word
			i := pos
			for i > 0 && buf[i-1] == ' ' {
				i--
			}
			for i > 0 && buf[i-1] != ' ' {
				i--
			}
			buf = append(buf[:i], buf[pos:]...)
			pos = i
		case 12: // Ctrl-L
			_, _ = out.WriteString("\x1b[H\x1b[2J")
		case 16, 14: // Ctrl-P / Ctrl-N
			s.historyMove(&histIdx, &saved, &buf, &pos, r == 16, setLine)
		case 27:
			s.escape(&buf, &pos, &histIdx, &saved, setLine)
		default:
			if r >= 32 {
				buf = append(buf, 0)
				copy(buf[pos+1:], buf[pos:])
				buf[pos] = r
				pos++
			}
		}
		redraw()
	}
}

func (s *Shell) historyMove(idx *int, saved *string, buf *[]rune, pos *int, up bool, setLine func(string)) {
	if up {
		if *idx == 0 {
			return
		}
		if *idx == len(s.history) {
			*saved = string(*buf)
		}
		*idx--
		setLine(s.history[*idx])
	} else {
		if *idx >= len(s.history) {
			return
		}
		*idx++
		if *idx == len(s.history) {
			setLine(*saved)
		} else {
			setLine(s.history[*idx])
		}
	}
}

// escape handles CSI / SS3 sequences (arrows, Home/End/Delete).
func (s *Shell) escape(buf *[]rune, pos, histIdx *int, saved *string, setLine func(string)) {
	r, _, err := s.stdin.ReadRune()
	if err != nil || (r != '[' && r != 'O') {
		return
	}
	var param []rune
	var final rune
	for {
		c, _, err := s.stdin.ReadRune()
		if err != nil {
			return
		}
		// Consume ECMA-48 parameter bytes (0x30..0x3F) and intermediate bytes (0x20..0x2F)
		if c >= 0x20 && c <= 0x3F {
			if len(param) < 32 {
				param = append(param, c)
			}
			continue
		}
		final = c
		break
	}
	switch final {
	case 'A':
		s.historyMove(histIdx, saved, buf, pos, true, setLine)
	case 'B':
		s.historyMove(histIdx, saved, buf, pos, false, setLine)
	case 'C':
		if *pos < len(*buf) {
			*pos++
		}
	case 'D':
		if *pos > 0 {
			*pos--
		}
	case 'H':
		*pos = 0
	case 'F':
		*pos = len(*buf)
	case '~':
		switch string(param) {
		case "1", "7":
			*pos = 0
		case "4", "8":
			*pos = len(*buf)
		case "3":
			if *pos < len(*buf) {
				*buf = append((*buf)[:*pos], (*buf)[*pos+1:]...)
			}
		}
	}
}
