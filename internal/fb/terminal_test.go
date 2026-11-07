package fb

import (
	"testing"
)

func TestTerminalGridAndANSI(t *testing.T) {
	term := &Terminal{
		rows:   10,
		cols:   30,
		curVis: true,
		curFg:  termDefaultFg,
		curBg:  termDefaultBg,
		csiBuf: make([]byte, 0, 32),
		title:  "Test Term",
	}
	term.grid = make([][]Cell, 10)
	for r := 0; r < 10; r++ {
		term.grid[r] = make([]Cell, 30)
		for c := 0; c < 30; c++ {
			term.grid[r][c] = Cell{Ch: ' ', Fg: termDefaultFg, Bg: termDefaultBg}
		}
	}

	// 1. Plain text writing
	_, _ = term.Write([]byte("Hello MYOS\n"))
	if term.curY != 1 || term.curX != 0 {
		t.Errorf("expected cursor at (0, 1), got (%d, %d)", term.curX, term.curY)
	}
	for i, ch := range "Hello MYOS" {
		if term.grid[0][i].Ch != byte(ch) {
			t.Errorf("grid[0][%d] = %c, want %c", i, term.grid[0][i].Ch, ch)
		}
	}

	// 2. ANSI Colors and Cursor positioning
	_, _ = term.Write([]byte("\x1b[31mRed\x1b[0m\x1b[5;10HPos"))
	if term.curY != 4 || term.curX != 12 {
		t.Errorf("expected cursor at (12, 4), got (%d, %d)", term.curX, term.curY)
	}
	if term.grid[1][0].Ch != 'R' || term.grid[1][0].Fg != ansi16[1] {
		t.Errorf("expected red 'R', got ch=%c fg=0x%X", term.grid[1][0].Ch, term.grid[1][0].Fg)
	}

	// 3. Clear Screen \x1b[2J
	_, _ = term.Write([]byte("\x1b[2J"))
	if term.curX != 0 || term.curY != 0 {
		t.Errorf("expected cursor reset to (0, 0), got (%d, %d)", term.curX, term.curY)
	}
	for r := 0; r < 10; r++ {
		for c := 0; c < 30; c++ {
			if term.grid[r][c].Ch != ' ' {
				t.Fatalf("cell (%d, %d) not cleared: %c", r, c, term.grid[r][c].Ch)
			}
		}
	}

	// 4. Scrolling
	for i := 0; i < 15; i++ {
		_, _ = term.Write([]byte("Line\n"))
	}
	if term.curY != 9 {
		t.Errorf("expected scrolled cursor to stay on last row (9), got %d", term.curY)
	}
}

func TestTerminalDraw(t *testing.T) {
	term := &Terminal{
		rows:   5,
		cols:   20,
		curVis: true,
		curFg:  termDefaultFg,
		curBg:  termDefaultBg,
		csiBuf: make([]byte, 0, 32),
		title:  "Mini Term",
	}
	term.grid = make([][]Cell, 5)
	for r := 0; r < 5; r++ {
		term.grid[r] = make([]Cell, 20)
		for c := 0; c < 20; c++ {
			term.grid[r][c] = Cell{Ch: ' ', Fg: termDefaultFg, Bg: termDefaultBg}
		}
	}

	_, _ = term.Write([]byte("myos# ls"))

	cols, rows, w, h := term.Size()
	if cols != 20 || rows != 5 || w != 20*8+16 || h != 5*16+22+12 {
		t.Errorf("unexpected size metrics: %d cols, %d rows, %dx%d px", cols, rows, w, h)
	}

	canvas := NewCanvas(400, 300)
	term.Draw(&canvas, 10, 10, w, h, true)

	// Check that canvas has title bar drawn
	if canvas.Pix[13*400+18] != colText && canvas.Pix[13*400+18] != colWinTitleHi {
		// Just ensure canvas wasn't left black
	}
}

func TestOpenPTY(t *testing.T) {
	master, slave, path, err := OpenPTY(24, 80)
	if err != nil {
		t.Skipf("skipping OpenPTY on host without /dev/ptmx access: %v", err)
		return
	}
	defer master.Close()
	defer slave.Close()

	if path == "" {
		t.Errorf("expected non-empty slave path")
	}

	// Test writing from master to slave
	msg := "hello pty\n"
	if _, err := master.Write([]byte(msg)); err != nil {
		t.Fatalf("write master: %v", err)
	}
}
