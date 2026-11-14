package shell

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"myos/internal/sys"
)

func termSize(fd int) (rows, cols int) {
	if fd >= 0 {
		var ws ttyWinsize
		if err := sys.Ioctl(fd, ioctlTIOCGWINSZ, unsafe.Pointer(&ws)); err == nil && ws.Row > 0 && ws.Col > 0 {
			return int(ws.Row), int(ws.Col)
		}
	}
	return 24, 80
}

// fdReady reports whether fd has data available within d without consuming bytes.
func fdReady(fd int, d time.Duration) bool {
	if fd < 0 || fd >= 1024 {
		return false
	}
	if d < 0 {
		d = 0
	}
	var fds syscall.FdSet
	fds.Bits[fd/64] |= 1 << (uint(fd) % 64)
	tv := syscall.NsecToTimeval(d.Nanoseconds())
	n, err := syscall.Select(fd+1, &fds, nil, nil, &tv)
	return err == nil && n > 0
}

type textEditor struct {
	path        string
	lines       [][]rune
	row, col    int
	rowOff      int
	colOff      int
	dirty       bool
	quitConfirm bool
	statusMsg   string
}

func newTextEditor(path string) (*textEditor, error) {
	ed := &textEditor{
		path:  path,
		lines: [][]rune{{}},
	}
	if err := ed.load(path); err != nil {
		return nil, err
	}
	return ed, nil
}

func (e *textEditor) load(path string) error {
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		return fmt.Errorf("%s: is a directory", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			e.lines = [][]rune{{}}
			e.statusMsg = "New file"
			return nil
		}
		return err
	}
	raw := strings.ReplaceAll(string(data), "\r\n", "\n")
	parts := strings.Split(raw, "\n")
	// Strip single trailing empty element caused by final newline so we don't
	// show a phantom extra blank line at EOF.
	if len(parts) > 1 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	e.lines = make([][]rune, len(parts))
	for i, p := range parts {
		e.lines[i] = []rune(p)
	}
	if len(e.lines) == 0 {
		e.lines = [][]rune{{}}
	}
	return nil
}

func (e *textEditor) save() (int, error) {
	if dir := filepath.Dir(e.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return 0, err
		}
	}
	var sb strings.Builder
	for i, line := range e.lines {
		sb.WriteString(string(line))
		if i < len(e.lines)-1 || len(line) > 0 || len(e.lines) > 1 {
			sb.WriteByte('\n')
		}
	}
	content := sb.String()
	if err := os.WriteFile(e.path, []byte(content), 0644); err != nil {
		return 0, err
	}
	e.dirty = false
	e.quitConfirm = false
	return len(content), nil
}

func (e *textEditor) clampCol() {
	if e.row < 0 {
		e.row = 0
	}
	if e.row >= len(e.lines) {
		e.row = len(e.lines) - 1
	}
	if e.col < 0 {
		e.col = 0
	}
	if e.col > len(e.lines[e.row]) {
		e.col = len(e.lines[e.row])
	}
}

func (e *textEditor) insertRune(r rune) {
	line := e.lines[e.row]
	next := make([]rune, len(line)+1)
	copy(next, line[:e.col])
	next[e.col] = r
	copy(next[e.col+1:], line[e.col:])
	e.lines[e.row] = next
	e.col++
	e.dirty = true
	e.quitConfirm = false
	e.statusMsg = ""
}

func (e *textEditor) insertNewline() {
	line := e.lines[e.row]
	left := make([]rune, e.col)
	copy(left, line[:e.col])
	right := make([]rune, len(line)-e.col)
	copy(right, line[e.col:])

	e.lines[e.row] = left
	e.lines = append(e.lines, nil)
	copy(e.lines[e.row+2:], e.lines[e.row+1:])
	e.lines[e.row+1] = right
	e.row++
	e.col = 0
	e.dirty = true
	e.quitConfirm = false
	e.statusMsg = ""
}

func (e *textEditor) backspace() {
	if e.col > 0 {
		line := e.lines[e.row]
		e.lines[e.row] = append(line[:e.col-1], line[e.col:]...)
		e.col--
		e.dirty = true
		e.quitConfirm = false
		e.statusMsg = ""
		return
	}
	if e.row > 0 {
		prev := e.lines[e.row-1]
		curr := e.lines[e.row]
		newCol := len(prev)
		merged := make([]rune, len(prev)+len(curr))
		copy(merged, prev)
		copy(merged[len(prev):], curr)
		e.lines[e.row-1] = merged
		e.lines = append(e.lines[:e.row], e.lines[e.row+1:]...)
		e.row--
		e.col = newCol
		e.dirty = true
		e.quitConfirm = false
		e.statusMsg = ""
	}
}

func (e *textEditor) deleteChar() {
	line := e.lines[e.row]
	if e.col < len(line) {
		e.lines[e.row] = append(line[:e.col], line[e.col+1:]...)
		e.dirty = true
		e.quitConfirm = false
		e.statusMsg = ""
		return
	}
	if e.row < len(e.lines)-1 {
		next := e.lines[e.row+1]
		merged := make([]rune, len(line)+len(next))
		copy(merged, line)
		copy(merged[len(line):], next)
		e.lines[e.row] = merged
		e.lines = append(e.lines[:e.row+1], e.lines[e.row+2:]...)
		e.dirty = true
		e.quitConfirm = false
		e.statusMsg = ""
	}
}

func (e *textEditor) moveLeft() {
	if e.col > 0 {
		e.col--
	} else if e.row > 0 {
		e.row--
		e.col = len(e.lines[e.row])
	}
}

func (e *textEditor) moveRight() {
	if e.col < len(e.lines[e.row]) {
		e.col++
	} else if e.row < len(e.lines)-1 {
		e.row++
		e.col = 0
	}
}

func (e *textEditor) moveUp() {
	if e.row > 0 {
		e.row--
		e.clampCol()
	}
}

func (e *textEditor) moveDown() {
	if e.row < len(e.lines)-1 {
		e.row++
		e.clampCol()
	}
}

func (e *textEditor) moveHome() { e.col = 0 }
func (e *textEditor) moveEnd()  { e.col = len(e.lines[e.row]) }

func (e *textEditor) pageUp(viewRows int) {
	if viewRows < 1 {
		viewRows = 1
	}
	e.row -= viewRows
	if e.row < 0 {
		e.row = 0
	}
	e.clampCol()
}

func (e *textEditor) pageDown(viewRows int) {
	if viewRows < 1 {
		viewRows = 1
	}
	e.row += viewRows
	if e.row >= len(e.lines) {
		e.row = len(e.lines) - 1
	}
	e.clampCol()
}

func (e *textEditor) scroll(viewRows, viewCols int) {
	if viewRows < 1 {
		viewRows = 1
	}
	if viewCols < 1 {
		viewCols = 1
	}
	if e.row < e.rowOff {
		e.rowOff = e.row
	}
	if e.row >= e.rowOff+viewRows {
		e.rowOff = e.row - viewRows + 1
	}
	if e.col < e.colOff {
		e.colOff = e.col
	}
	if e.col >= e.colOff+viewCols {
		e.colOff = e.col - viewCols + 1
	}
}

func (e *textEditor) render(rows, cols int) string {
	if rows < 2 {
		rows = 2
	}
	if cols < 10 {
		cols = 10
	}
	viewRows := rows - 1
	e.scroll(viewRows, cols)

	var sb strings.Builder
	sb.Grow(rows * (cols + 16))
	sb.WriteString("\x1b[?25l\x1b[H")

	for y := 0; y < viewRows; y++ {
		fileRow := e.rowOff + y
		sb.WriteString("\x1b[K")
		if fileRow < len(e.lines) {
			line := e.lines[fileRow]
			if e.colOff < len(line) {
				end := min(e.colOff+cols, len(line))
				for _, r := range line[e.colOff:end] {
					if r == '\t' {
						sb.WriteByte(' ')
					} else {
						sb.WriteRune(r)
					}
				}
			}
		} else {
			sb.WriteString("\x1b[90m~\x1b[0m")
		}
		sb.WriteString("\r\n")
	}

	// Bottom status bar (row == rows)
	dirtyTag := ""
	if e.dirty {
		dirtyTag = " [Modified]"
	}
	left := fmt.Sprintf(" Ctrl+S: Save | Ctrl+Q: Quit | %s%s", filepath.Base(e.path), dirtyTag)
	if e.statusMsg != "" {
		left = fmt.Sprintf(" %s | %s%s", e.statusMsg, filepath.Base(e.path), dirtyTag)
	}
	right := fmt.Sprintf("Ln %d, Col %d ", e.row+1, e.col+1)

	leftRunes := []rune(left)
	rightRunes := []rune(right)
	var bar strings.Builder
	if len(leftRunes)+len(rightRunes) >= cols {
		maxLeft := cols - len(rightRunes) - 1
		if maxLeft < 0 {
			maxLeft = 0
		}
		if len(leftRunes) > maxLeft {
			leftRunes = leftRunes[:maxLeft]
		}
		bar.WriteString(string(leftRunes))
		pad := cols - len(leftRunes) - len(rightRunes)
		for i := 0; i < pad; i++ {
			bar.WriteByte(' ')
		}
		if len(rightRunes) <= cols {
			bar.WriteString(string(rightRunes))
		}
	} else {
		bar.WriteString(string(leftRunes))
		pad := cols - len(leftRunes) - len(rightRunes)
		for i := 0; i < pad; i++ {
			bar.WriteByte(' ')
		}
		bar.WriteString(string(rightRunes))
	}

	sb.WriteString("\x1b[7m")
	sb.WriteString(bar.String())
	sb.WriteString("\x1b[0m")

	curRow := (e.row - e.rowOff) + 1
	curCol := (e.col - e.colOff) + 1
	fmt.Fprintf(&sb, "\x1b[%d;%dH\x1b[?25h", curRow, curCol)
	return sb.String()
}

func readEditorEscape(r *bufio.Reader, inFd int) string {
	if r.Buffered() == 0 && !fdReady(inFd, 25*time.Millisecond) {
		return ""
	}
	b, err := r.ReadByte()
	if err != nil {
		return ""
	}
	if b != '[' && b != 'O' {
		return string(b)
	}
	var seq []byte
	seq = append(seq, b)
	for len(seq) < 8 {
		if r.Buffered() == 0 && !fdReady(inFd, 15*time.Millisecond) {
			break
		}
		nb, err := r.ReadByte()
		if err != nil {
			break
		}
		seq = append(seq, nb)
		if (nb >= 'A' && nb <= 'Z') || (nb >= 'a' && nb <= 'z') || nb == '~' {
			break
		}
	}
	return string(seq)
}

func biEdit(s *Shell, io *IO, a []string) int {
	if len(a) != 2 {
		fmt.Fprintln(io.Err, "usage: edit <filepath>")
		return 1
	}
	ed, err := newTextEditor(a[1])
	if err != nil {
		fmt.Fprintln(io.Err, "edit:", err)
		return 1
	}

	inFile := s.inFile()
	outFile := s.outFile()
	inFd := int(inFile.Fd())
	outFd := int(outFile.Fd())

	if !isTTY(inFd) {
		fmt.Fprintln(io.Err, "edit: requires an interactive terminal")
		return 1
	}
	restore, err := makeRaw(inFd)
	if err != nil {
		fmt.Fprintln(io.Err, "edit:", err)
		return 1
	}
	defer restore()

	_, _ = outFile.WriteString("\x1b[?1049h")
	defer func() {
		_, _ = outFile.WriteString("\x1b[?25h\x1b[?1049l")
	}()

	reader := s.stdin
	if reader == nil {
		reader = bufio.NewReader(inFile)
	}

	for {
		rows, cols := termSize(outFd)
		if rows == 24 && cols == 80 {
			rows, cols = termSize(inFd)
		}
		_, _ = outFile.WriteString(ed.render(rows, cols))

		r, _, err := reader.ReadRune()
		if err != nil {
			if errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EAGAIN) {
				continue
			}
			return 0
		}

		switch r {
		case 17: // Ctrl+Q
			if ed.dirty && !ed.quitConfirm {
				ed.quitConfirm = true
				ed.statusMsg = "Unsaved! Ctrl+Q again to quit, Ctrl+S to save"
				continue
			}
			return 0
		case 19: // Ctrl+S
			n, saveErr := ed.save()
			if saveErr != nil {
				ed.statusMsg = fmt.Sprintf("Save error: %v", saveErr)
			} else {
				ed.statusMsg = fmt.Sprintf("Saved %d bytes", n)
			}
		case '\r', '\n':
			ed.insertNewline()
		case 127, 8: // Backspace
			ed.backspace()
		case 4: // Ctrl+D -> Delete
			ed.deleteChar()
		case '\t':
			for i := 0; i < 4; i++ {
				ed.insertRune(' ')
			}
		case 27: // Escape sequence
			seq := readEditorEscape(reader, inFd)
			viewRows := rows - 1
			switch seq {
			case "[A":
				ed.moveUp()
			case "[B":
				ed.moveDown()
			case "[C":
				ed.moveRight()
			case "[D":
				ed.moveLeft()
			case "[H", "[1~", "OH":
				ed.moveHome()
			case "[F", "[4~", "OF":
				ed.moveEnd()
			case "[3~":
				ed.deleteChar()
			case "[5~":
				ed.pageUp(viewRows)
			case "[6~":
				ed.pageDown(viewRows)
			}
		default:
			if r >= 32 && r != utf8.RuneError {
				ed.insertRune(r)
			}
		}
	}
}
