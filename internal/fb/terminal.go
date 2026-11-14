package fb

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"myos/internal/proc"
)

const (
	ioctlTIOCSWINSZ  = 0x00005414
	ioctlTIOCSPTLCK  = 0x40045431
	ioctlTIOCGPTN    = 0x80045430
	ioctlTIOCGPTPEER = 0x00005441
)

type Winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

type Cell struct {
	Ch rune
	Fg Color
	Bg Color
}

const (
	termDefaultBg Color = 0x0C101C // dark navy black
	termDefaultFg Color = 0xD0D7DE // soft white
	termCursorCol Color = 0x4C8DFF // accent blue cursor
)

var ansi16 = [16]Color{
	0x0C101C, // 0: Black
	0xEA4A5A, // 1: Red
	0x34D058, // 2: Green
	0xFFB454, // 3: Yellow
	0x2188FF, // 4: Blue
	0xB392F0, // 5: Magenta
	0x39C5BB, // 6: Cyan
	0xD0D7DE, // 7: White
	0x57606A, // 8: Bright Black (Gray)
	0xFF7B72, // 9: Bright Red
	0x56D364, // 10: Bright Green
	0xE3B341, // 11: Bright Yellow
	0x79C0FF, // 12: Bright Blue
	0xD2A8FF, // 13: Bright Magenta
	0x56D4DD, // 14: Bright Cyan
	0xF0F6FC, // 15: Bright White
}

// Terminal represents an interactive graphical PTY terminal emulator.
type Terminal struct {
	mu       sync.Mutex
	master   *os.File
	slave    *os.File
	childPid int
	rows     int
	cols     int
	grid     [][]Cell
	curX     int
	curY     int
	curVis   bool
	active   bool
	closed   bool
	dirty    bool
	title    string
	slaveDev string

	// ANSI & UTF-8 parser state
	inEsc  bool
	inCSI  bool
	inOSC  bool
	csiBuf []byte
	utfBuf [4]byte
	utfLen int
	curFg  Color
	curBg  Color
}

// OpenPTY allocates a Linux pseudo-terminal pair and sets its window size.
func OpenPTY(rows, cols int) (*os.File, *os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		master, err = os.OpenFile("/dev/pts/ptmx", os.O_RDWR|syscall.O_CLOEXEC, 0)
		if err != nil {
			return nil, nil, "", fmt.Errorf("open /dev/ptmx: %w", err)
		}
	}

	var unlock int32 = 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), uintptr(ioctlTIOCSPTLCK), uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		master.Close()
		return nil, nil, "", fmt.Errorf("ioctl TIOCSPTLCK: %w", errno)
	}

	var slave *os.File
	slavePath := ""

	// Try TIOCGPTPEER first (Linux 4.13+)
	peerFd, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), uintptr(ioctlTIOCGPTPEER), uintptr(os.O_RDWR|syscall.O_CLOEXEC))
	if errno == 0 {
		slave = os.NewFile(peerFd, "/dev/pts/slave")
		slavePath = "/dev/pts/slave"
	} else {
		// Fallback to TIOCGPTN + /dev/pts/N
		var ptyNum uint32
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), uintptr(ioctlTIOCGPTN), uintptr(unsafe.Pointer(&ptyNum))); errno != 0 {
			master.Close()
			return nil, nil, "", fmt.Errorf("ioctl TIOCGPTN: %w", errno)
		}
		slavePath = fmt.Sprintf("/dev/pts/%d", ptyNum)
		slave, err = os.OpenFile(slavePath, os.O_RDWR|syscall.O_CLOEXEC, 0)
		if err != nil {
			master.Close()
			return nil, nil, "", fmt.Errorf("open %s: %w", slavePath, err)
		}
	}

	ws := Winsize{
		Row:    uint16(rows),
		Col:    uint16(cols),
		Xpixel: uint16(cols * GlyphW),
		Ypixel: uint16(rows * GlyphH),
	}
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), uintptr(ioctlTIOCSWINSZ), uintptr(unsafe.Pointer(&ws)))

	return master, slave, slavePath, nil
}

// NewTerminal instantiates a terminal emulator with rows and cols grid size.
func NewTerminal(rows, cols int) (*Terminal, error) {
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}

	t := &Terminal{
		rows:   rows,
		cols:   cols,
		curVis: true,
		curFg:  termDefaultFg,
		curBg:  termDefaultBg,
		csiBuf: make([]byte, 0, 32),
		title:  "Terminal",
	}

	t.grid = make([][]Cell, rows)
	for r := 0; r < rows; r++ {
		t.grid[r] = make([]Cell, cols)
		for c := 0; c < cols; c++ {
			t.grid[r][c] = Cell{Ch: ' ', Fg: t.curFg, Bg: t.curBg}
		}
	}

	master, slave, slavePath, err := OpenPTY(rows, cols)
	if err != nil {
		return nil, err
	}
	t.master = master
	t.slave = slave
	t.slaveDev = slavePath
	t.title = fmt.Sprintf("Terminal (%s)", slavePath)
	t.active = true

	// Fork/exec shell attached to slave PTY
	t.startProcess()

	// Asynchronous reader from master PTY into terminal buffer
	go t.readLoop()

	return t, nil
}

// ShellWorker is an optional callback to start an internal in-process shell worker
// attached to the slave PTY if fork/exec fails or is running in a test harness.
var ShellWorker func(slave *os.File)

func (t *Terminal) startProcess() {
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "/init"
	}

	// In unit test runner binary, don't re-exec the test harness; use worker if available
	if strings.HasSuffix(self, ".test") {
		if ShellWorker != nil {
			slave := t.slave
			t.slave = nil
			go func() {
				defer func() {
					time.Sleep(15 * time.Millisecond)
					_ = slave.Close()
				}()
				ShellWorker(slave)
				t.mu.Lock()
				t.active = false
				t.mu.Unlock()
			}()
		}
		return
	}

	env := append(os.Environ(),
		"TERM=linux",
		fmt.Sprintf("LINES=%d", t.rows),
		fmt.Sprintf("COLUMNS=%d", t.cols),
	)
	sysAttr := &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0, // Child's stdin file descriptor index (0), NOT parent's slave.Fd()!
	}
	slaveFd := t.slave.Fd()
	pid, err := proc.SpawnAttr(self, []string{self, "-shell"}, env, []uintptr{slaveFd, slaveFd, slaveFd}, sysAttr)
	if err != nil {
		_, _ = t.WriteString(fmt.Sprintf("\r\n[terminal] exec %s -shell: %v\r\n", self, err))
		if ShellWorker != nil {
			_, _ = t.WriteString("[terminal] starting internal shell worker...\r\n")
			slave := t.slave
			t.slave = nil
			go func() {
				defer func() {
					time.Sleep(15 * time.Millisecond)
					_ = slave.Close()
				}()
				ShellWorker(slave)
				t.mu.Lock()
				t.active = false
				t.mu.Unlock()
			}()
		} else {
			if t.slave != nil {
				_ = t.slave.Close()
				t.slave = nil
			}
			t.mu.Lock()
			t.active = false
			t.mu.Unlock()
		}
		return
	}

	// Close parent's copy of slave FD immediately so master.Read receives EIO on child exit
	if t.slave != nil {
		_ = t.slave.Close()
		t.slave = nil
	}

	t.childPid = pid
	go func(pid int) {
		_ = proc.Wait(pid)
		t.mu.Lock()
		t.active = false
		t.mu.Unlock()
	}(pid)
}

func (t *Terminal) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := t.master.Read(buf)
		if n > 0 {
			t.Write(buf[:n])
		}
		if err != nil {
			t.mu.Lock()
			t.active = false
			t.mu.Unlock()
			return
		}
	}
}

// WriteInput sends keystrokes into the master PTY for the child process.
// It releases t.mu before calling master.Write so it cannot block readLoop or Draw.
func (t *Terminal) WriteInput(data []byte) (int, error) {
	t.mu.Lock()
	if t.closed || t.master == nil {
		t.mu.Unlock()
		return 0, errors.New("terminal closed")
	}
	master := t.master
	t.mu.Unlock()

	return master.Write(data)
}

// WriteString writes a string directly into the terminal display grid under mutex.
func (t *Terminal) WriteString(s string) (int, error) {
	return t.Write([]byte(s))
}

const maxCSIBufLen = 64

// Write parses ANSI escape sequences, decodes UTF-8 runes, and writes printable characters into the grid buffer.
func (t *Terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dirty = true

	for i := 0; i < len(p); i++ {
		b := p[i]

		if t.inOSC {
			if t.inEsc && b == '\\' { // ST (\x1b\\) terminates OSC
				t.inOSC = false
				t.inEsc = false
				continue
			}
			if b == 7 { // BEL (\x07) terminates OSC
				t.inOSC = false
				t.inEsc = false
				continue
			}
			t.inEsc = (b == 27)
			continue
		}

		if t.inCSI {
			// ECMA-48 parameter bytes (0x30..0x3F) and intermediate bytes (0x20..0x2F)
			if b >= 0x20 && b <= 0x3F {
				if len(t.csiBuf) >= maxCSIBufLen {
					// Cap CSI buffer to prevent unbounded allocation in PID 1
					t.csiBuf = t.csiBuf[:0]
					t.inCSI = false
					t.inEsc = false
					continue
				}
				t.csiBuf = append(t.csiBuf, b)
				continue
			}
			// Final byte of CSI sequence (0x40..0x7E)
			if b >= 0x40 && b <= 0x7E {
				t.handleCSI(b)
			}
			t.inCSI = false
			t.inEsc = false
			continue
		}

		if t.inEsc {
			switch b {
			case '[':
				t.inCSI = true
				t.csiBuf = t.csiBuf[:0]
			case ']':
				t.inOSC = true
				t.inEsc = false
			default:
				// Unhandled two-byte escape sequence
				t.inEsc = false
			}
			continue
		}

		if b >= 0x80 {
			t.utfBuf[t.utfLen] = b
			t.utfLen++
			if utf8.FullRune(t.utfBuf[:t.utfLen]) {
				r, _ := utf8.DecodeRune(t.utfBuf[:t.utfLen])
				t.utfLen = 0
				t.putRune(r)
			} else if t.utfLen >= len(t.utfBuf) {
				t.utfLen = 0
				t.putRune(utf8.RuneError)
			}
			continue
		}
		t.utfLen = 0

		switch b {
		case 27: // ESC
			t.inEsc = true
		case '\r':
			t.curX = 0
		case '\n':
			t.curX = 0
			t.curY++
			if t.curY >= t.rows {
				t.scrollUp()
			}
		case '\b', 127: // Backspace
			if t.curX > 0 {
				t.curX--
			}
		case '\t':
			nextTab := (t.curX + 8) &^ 7
			if nextTab >= t.cols {
				nextTab = t.cols - 1
			}
			t.curX = nextTab
		case '\a': // Bell - swallow
		default:
			if b >= 32 {
				t.putRune(rune(b))
			}
		}
	}
	return len(p), nil
}

func (t *Terminal) putRune(r rune) {
	if t.curX >= t.cols {
		t.curX = 0
		t.curY++
		if t.curY >= t.rows {
			t.scrollUp()
		}
	}
	if t.curY >= 0 && t.curY < t.rows && t.curX >= 0 && t.curX < t.cols {
		t.grid[t.curY][t.curX] = Cell{Ch: r, Fg: t.curFg, Bg: t.curBg}
	}
	t.curX++
}

func (t *Terminal) scrollUp() {
	for r := 1; r < t.rows; r++ {
		copy(t.grid[r-1], t.grid[r])
	}
	last := t.grid[t.rows-1]
	for c := 0; c < t.cols; c++ {
		last[c] = Cell{Ch: ' ', Fg: t.curFg, Bg: t.curBg}
	}
	t.curY = t.rows - 1
}

func (t *Terminal) handleCSI(final byte) {
	// Ignore private/intermediate sequences that have intermediate bytes (0x20..0x2F)
	for _, b := range t.csiBuf {
		if b >= 0x20 && b <= 0x2F {
			return
		}
	}
	params := string(t.csiBuf)
	args := parseCSIArgs(params)

	switch final {
	case 'J': // Erase in display
		mode := 0
		if len(args) > 0 {
			mode = args[0]
		}
		if mode == 2 || mode == 3 { // Clear entire screen
			for r := 0; r < t.rows; r++ {
				for c := 0; c < t.cols; c++ {
					t.grid[r][c] = Cell{Ch: ' ', Fg: t.curFg, Bg: t.curBg}
				}
			}
			t.curX, t.curY = 0, 0
		}
	case 'H', 'f': // Cursor position (1-based: row;col)
		r, c := 1, 1
		if len(args) > 0 && args[0] > 0 {
			r = args[0]
		}
		if len(args) > 1 && args[1] > 0 {
			c = args[1]
		}
		t.curY = min(max(r-1, 0), t.rows-1)
		t.curX = min(max(c-1, 0), t.cols-1)
	case 'K': // Erase in line
		mode := 0
		if len(args) > 0 {
			mode = args[0]
		}
		switch mode {
		case 0: // Clear from cursor to end of line
			for c := t.curX; c < t.cols; c++ {
				t.grid[t.curY][c] = Cell{Ch: ' ', Fg: t.curFg, Bg: t.curBg}
			}
		case 1: // Clear from start to cursor
			for c := 0; c <= t.curX && c < t.cols; c++ {
				t.grid[t.curY][c] = Cell{Ch: ' ', Fg: t.curFg, Bg: t.curBg}
			}
		case 2: // Clear entire line
			for c := 0; c < t.cols; c++ {
				t.grid[t.curY][c] = Cell{Ch: ' ', Fg: t.curFg, Bg: t.curBg}
			}
		}
	case 'A': // Cursor Up
		n := 1
		if len(args) > 0 && args[0] > 0 {
			n = args[0]
		}
		t.curY = max(0, t.curY-n)
	case 'B': // Cursor Down
		n := 1
		if len(args) > 0 && args[0] > 0 {
			n = args[0]
		}
		t.curY = min(t.rows-1, t.curY+n)
	case 'C': // Cursor Forward (Right)
		n := 1
		if len(args) > 0 && args[0] > 0 {
			n = args[0]
		}
		t.curX = min(t.cols-1, t.curX+n)
	case 'D': // Cursor Back (Left)
		n := 1
		if len(args) > 0 && args[0] > 0 {
			n = args[0]
		}
		t.curX = max(0, t.curX-n)
	case 'm': // SGR (Select Graphic Rendition)
		if len(args) == 0 {
			args = []int{0}
		}
		for _, a := range args {
			switch {
			case a == 0: // Reset
				t.curFg = termDefaultFg
				t.curBg = termDefaultBg
			case a >= 30 && a <= 37:
				t.curFg = ansi16[a-30]
			case a >= 90 && a <= 97:
				t.curFg = ansi16[a-90+8]
			case a >= 40 && a <= 47:
				t.curBg = ansi16[a-40]
			case a >= 100 && a <= 107:
				t.curBg = ansi16[a-100+8]
			}
		}
	case 'h': // Set Mode
		if params == "?25" {
			t.curVis = true
		}
	case 'l': // Reset Mode
		if params == "?25" {
			t.curVis = false
		}
	}
}

func parseCSIArgs(params string) []int {
	params = strings.TrimPrefix(params, "?")
	if params == "" {
		return nil
	}
	parts := strings.Split(params, ";")
	res := make([]int, 0, len(parts))
	for _, p := range parts {
		if v, err := strconv.Atoi(p); err == nil {
			res = append(res, v)
		} else {
			res = append(res, 0)
		}
	}
	return res
}

// Draw renders the terminal window onto the canvas.
func (t *Terminal) Draw(c *Canvas, winX, winY, winW, winH int, focused bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dirty = false

	// Window shadow
	c.DrawFillRect(winX+5, winY+5, winW, winH, 0x060C1C)

	// Window body
	c.DrawFillRect(winX, winY, winW, winH, termDefaultBg)

	// Title bar
	titleCol := colWinTitle
	if focused {
		titleCol = colWinTitleHi
	}
	c.DrawFillRect(winX, winY, winW, titleBarH, titleCol)
	c.DrawString(winX+8, winY+3, t.title, colText)

	// Title bar close button [X]
	btnX := winX + winW - 20
	c.DrawString(btnX, winY+3, "X", colText)

	// Window border
	c.DrawRect(winX, winY, winW, winH, colTopBarLine)

	// Content area offset
	contentX := winX + 8
	contentY := winY + titleBarH + 6

	for r := 0; r < t.rows; r++ {
		rowY := contentY + r*GlyphH
		if rowY+GlyphH > winY+winH {
			break
		}
		for col := 0; col < t.cols; col++ {
			colX := contentX + col*GlyphW
			if colX+GlyphW > winX+winW {
				break
			}
			cell := t.grid[r][col]
			if cell.Bg != termDefaultBg {
				c.DrawFillRect(colX, rowY, GlyphW, GlyphH, cell.Bg)
			}
			if cell.Ch > 32 {
				ch := byte('?')
				if cell.Ch <= 126 {
					ch = byte(cell.Ch)
				}
				c.DrawGlyph(colX, rowY, ch, cell.Fg)
			}
		}
	}

	// Render cursor
	if t.curVis && t.curY >= 0 && t.curY < t.rows && t.curX >= 0 && t.curX < t.cols {
		cursorX := contentX + t.curX*GlyphW
		cursorY := contentY + t.curY*GlyphH
		if cursorX+GlyphW <= winX+winW && cursorY+GlyphH <= winY+winH {
			if focused {
				c.DrawFillRect(cursorX, cursorY, GlyphW, GlyphH, termCursorCol)
				curCell := t.grid[t.curY][t.curX]
				if curCell.Ch > 32 {
					ch := byte('?')
					if curCell.Ch <= 126 {
						ch = byte(curCell.Ch)
					}
					c.DrawGlyph(cursorX, cursorY, ch, 0x000000)
				}
			} else {
				c.DrawRect(cursorX, cursorY, GlyphW, GlyphH, termCursorCol)
			}
		}
	}
}

// IsActive reports whether the child shell is running and PTY is active.
func (t *Terminal) IsActive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.active && !t.closed
}

// IsDirty reports whether the terminal grid has changed since the last Draw.
func (t *Terminal) IsDirty() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dirty
}

// Close terminates child process and releases PTY descriptors.
func (t *Terminal) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	t.active = false
	if t.childPid > 0 {
		_ = syscall.Kill(t.childPid, syscall.SIGKILL)
	}
	if t.master != nil {
		_ = t.master.Close()
	}
	if t.slave != nil {
		_ = t.slave.Close()
	}
	return nil
}

// Size returns cols, rows, and pixel width/height needed for the window.
func (t *Terminal) Size() (cols, rows, width, height int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	w := t.cols*GlyphW + 16
	h := t.rows*GlyphH + titleBarH + 12
	return t.cols, t.rows, w, h
}
