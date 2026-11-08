package fb

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"myos/internal/input"
	"myos/internal/monitor"
)

// Palette.
const (
	colBackground Color = 0x0B1530 // dark navy
	colTopBar     Color = 0x16254A
	colTopBarLine Color = 0x2B4180
	colText       Color = 0xE6EDF7
	colDim        Color = 0x8FA3C7
	colAccent     Color = 0x4C8DFF
	colWinBody    Color = 0x1B2744
	colWinTitle   Color = 0x2F5BC0
	colWinTitleHi Color = 0x4C8DFF
	colButton     Color = 0x3A5FA8
	colButtonDown Color = 0x20366A
)

const (
	topBarH   = 24
	titleBarH = 22
)

// Device is the display hardware interface for both DRM/KMS and Framebuffer.
type Device interface {
	Width() int
	Height() int
	Surface() *Canvas
	Blit()
	Close() error
	Backend() string
}

// Window is a movable window rectangle.
type Window struct {
	X, Y, W, H int
	Title      string
}

// Desktop is the desktop environment containing windows, topbar, and terminal.
type Desktop struct {
	Win     Window // System Monitor window
	TermWin Window // Terminal window
	Term    *Terminal

	width       int
	height      int
	focusWin    int // 0: Win (Sysmon), 1: TermWin (Terminal)
	draggingWin int // -1: none, 0: Win, 1: TermWin
	dragDX      int
	dragDY      int
	shouldQuit  bool

	clicks   uint64
	frames   uint64
	fps      int
	fpsCount int
	fpsStamp time.Time

	prevBtn uint8

	statusAt   time.Time
	barRight   []byte // "MEM a/b MB  IP x.x.x.x" refreshed once a second
	clock      []byte
	scratch    []byte
	memUsedMB  uint64
	memTotalMB uint64
	cpuPercent float64
	prevCPU    monitor.CPUTimes
}

// NewDesktop creates a desktop with both System Monitor and Terminal windows.
func NewDesktop(w, h int) *Desktop {
	// Terminal window dimensions: 70 cols x 20 rows
	termCols, termRows := 70, 20
	termW := termCols*GlyphW + 16
	termH := termRows*GlyphH + titleBarH + 12
	if termW > w-40 {
		termCols = max((w-56)/GlyphW, 40)
		termW = termCols*GlyphW + 16
	}
	if termH > h-topBarH-20 {
		termRows = max((h-topBarH-44)/GlyphH, 10)
		termH = termRows*GlyphH + titleBarH + 12
	}

	termX := 60
	termY := topBarH + 32

	// System monitor window
	sysW, sysH := 420, 240
	sysX := termX + termW + 20
	sysY := termY
	if sysX+sysW > w-10 {
		sysX = max(w-sysW-20, 20)
		sysY = max(h-sysH-20, topBarH+10)
	}

	d := &Desktop{
		Win:         Window{X: sysX, Y: sysY, W: sysW, H: sysH, Title: "System Monitor"},
		TermWin:     Window{X: termX, Y: termY, W: termW, H: termH, Title: "Terminal"},
		width:       w,
		height:      h,
		focusWin:    1, // Default focus to terminal
		draggingWin: -1,
		fpsStamp:    time.Now(),
		scratch:     make([]byte, 0, 128),
	}

	term, err := NewTerminal(termRows, termCols)
	if err == nil {
		d.Term = term
		d.TermWin.Title = term.title
	} else {
		fmt.Fprintf(os.Stderr, "[desktop] terminal init: %v\n", err)
	}

	return d
}

func (d *Desktop) buttonRect() (x, y, w, h int) {
	return d.Win.X + 16, d.Win.Y + titleBarH + 112, 140, 30
}

func (d *Desktop) exitButtonRect(screenW int) (x, y, w, h int) {
	if screenW <= 0 {
		screenW = d.width
	}
	if screenW <= 0 {
		screenW = 1280
	}
	return screenW - 68, 2, 60, 20
}

// ExitButtonRect returns the topbar [Exit] button rectangle for a given screen width.
func (d *Desktop) ExitButtonRect(screenW int) (x, y, w, h int) {
	return d.exitButtonRect(screenW)
}

func inside(px, py, x, y, w, h int) bool { return px >= x && py >= y && px < x+w && py < y+h }

// RouteInput routes keyboard bytes to the terminal or handles quit shortcuts.
// Keystrokes are forwarded directly to Terminal.WriteInput, supporting printable
// runes, control keys, Backspace (0x7F / 0x08), Enter (0x0D / 0x0A), and arrow escape sequences.
func (d *Desktop) RouteInput(data []byte) bool {
	for _, b := range data {
		if b == 0x11 { // Ctrl-Q exits desktop
			d.shouldQuit = true
			return true
		}
	}
	if d.Term != nil && d.Term.IsActive() && d.focusWin == 1 {
		_, _ = d.Term.WriteInput(data)
		return false
	}
	// Fallback when terminal not focused or not active
	for _, b := range data {
		if b == 'q' || b == 'Q' || b == 27 || b == 3 {
			d.shouldQuit = true
			return true
		}
	}
	if d.Term != nil && d.Term.IsActive() {
		_, _ = d.Term.WriteInput(data)
	}
	return false
}

// RouteKey routes keyboard input from the GUI loop to the terminal or handles quit shortcuts.
func (d *Desktop) RouteKey(k byte) bool {
	return d.RouteInput([]byte{k})
}

// Update handles mouse interaction (window dragging, button clicks, focus) for one frame.
func (d *Desktop) Update(st input.State) {
	pressed := st.Buttons&input.Left != 0 && d.prevBtn&input.Left == 0
	released := st.Buttons&input.Left == 0

	sw := st.W
	if sw <= 0 || (d.width > 0 && sw != d.width) {
		sw = d.width
	}

	if pressed {
		// 1. Check Topbar Exit Button
		ex, ey, ew, eh := d.exitButtonRect(sw)
		if inside(st.X, st.Y, ex, ey, ew, eh) {
			d.shouldQuit = true
			return
		}

		// 2. Check Terminal Window
		if d.Term != nil {
			closeBtnX := d.TermWin.X + d.TermWin.W - 20
			if inside(st.X, st.Y, closeBtnX, d.TermWin.Y+2, 18, 18) {
				_ = d.Term.Close()
				d.Term = nil
			} else if inside(st.X, st.Y, d.TermWin.X, d.TermWin.Y, d.TermWin.W, titleBarH) {
				d.focusWin = 1
				d.draggingWin = 1
				d.dragDX, d.dragDY = st.X-d.TermWin.X, st.Y-d.TermWin.Y
			} else if inside(st.X, st.Y, d.TermWin.X, d.TermWin.Y, d.TermWin.W, d.TermWin.H) {
				d.focusWin = 1
			}
		}

		// 3. Check System Monitor Window
		w := &d.Win
		if inside(st.X, st.Y, w.X, w.Y, w.W, titleBarH) {
			d.focusWin = 0
			d.draggingWin = 0
			d.dragDX, d.dragDY = st.X-w.X, st.Y-w.Y
		} else if inside(st.X, st.Y, w.X, w.Y, w.W, w.H) {
			d.focusWin = 0
			if bx, by, bw, bh := d.buttonRect(); inside(st.X, st.Y, bx, by, bw, bh) {
				d.clicks++
			}
		}
	}

	if released {
		d.draggingWin = -1
	}

	// Update window positions during drag
	if d.draggingWin == 0 {
		w := &d.Win
		w.X = min(max(st.X-d.dragDX, 0), max(sw-w.W, 0))
		w.Y = min(max(st.Y-d.dragDY, topBarH), max(st.H-w.H, topBarH))
	} else if d.draggingWin == 1 {
		tw := &d.TermWin
		tw.X = min(max(st.X-d.dragDX, 0), max(sw-tw.W, 0))
		tw.Y = min(max(st.Y-d.dragDY, topBarH), max(st.H-tw.H, topBarH))
	}

	d.prevBtn = st.Buttons

	now := time.Now()
	d.fpsCount++
	if now.Sub(d.fpsStamp) >= time.Second {
		d.fps, d.fpsCount, d.fpsStamp = d.fpsCount, 0, now
	}
	if now.Sub(d.statusAt) >= time.Second || d.statusAt.IsZero() {
		d.refreshStatus(now)
	}
	d.frames++
}

// refreshStatus rebuilds the top-bar strings; runs about once per second.
func (d *Desktop) refreshStatus(now time.Time) {
	d.statusAt = now
	d.clock = now.AppendFormat(d.clock[:0], "15:04:05")
	b := d.barRight[:0]
	b = append(b, "MEM "...)
	if m, err := monitor.ReadMemInfo(); err == nil {
		d.memUsedMB = m.UsedKB() / 1024
		d.memTotalMB = m.TotalKB / 1024
		b = strconv.AppendUint(b, d.memUsedMB, 10)
		b = append(b, '/')
		b = strconv.AppendUint(b, d.memTotalMB, 10)
		b = append(b, " MB"...)
	} else {
		b = append(b, "n/a"...)
	}
	b = append(b, "   IP "...)
	b = append(b, primaryIP()...)
	d.barRight = b

	if curCPU, err := monitor.ReadCPUTimes(); err == nil {
		if d.prevCPU.Total > 0 {
			d.cpuPercent = monitor.CPUPercent(d.prevCPU, curCPU)
		}
		d.prevCPU = curCPU
	}
}

func primaryIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "n/a"
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
			if v4 := ipn.IP.To4(); v4 != nil {
				return v4.String()
			}
		}
	}
	return "no link"
}

// Draw renders the whole desktop scene into c.
func (d *Desktop) Draw(c *Canvas, st input.State, backend ...string) {
	if c != nil && c.W > 0 && c.H > 0 {
		d.width, d.height = c.W, c.H
	}
	c.DrawFillRect(0, 0, c.W, c.H, colBackground)

	// Top bar: title on the left, clock centred, memory/IP on the right, exit button.
	c.DrawFillRect(0, 0, c.W, topBarH, colTopBar)
	c.DrawFillRect(0, topBarH-1, c.W, 1, colTopBarLine)
	c.DrawString(8, 4, "myos", colAccent)
	c.DrawBytes((c.W-len(d.clock)*GlyphW)/2, 4, d.clock, colText)
	c.DrawBytes(c.W-len(d.barRight)*GlyphW-76, 4, d.barRight, colDim)

	// Exit button on topbar
	ex, ey, ew, eh := d.exitButtonRect(c.W)
	c.DrawFillRect(ex, ey, ew, eh, colButton)
	c.DrawRect(ex, ey, ew, eh, colAccent)
	c.DrawString(ex+14, ey+2, "Exit", colText)

	// Render windows according to focus order (focused window rendered last/on top)
	if d.focusWin == 1 {
		d.drawSysmon(c, st, backend...)
		d.drawTerminal(c)
	} else {
		d.drawTerminal(c)
		d.drawSysmon(c, st, backend...)
	}
}

func (d *Desktop) drawTerminal(c *Canvas) {
	if d.Term == nil {
		return
	}
	tw := &d.TermWin
	d.Term.Draw(c, tw.X, tw.Y, tw.W, tw.H, d.focusWin == 1)
}

func (d *Desktop) drawSysmon(c *Canvas, st input.State, backend ...string) {
	w := &d.Win
	c.DrawFillRect(w.X+5, w.Y+5, w.W, w.H, 0x060C1C)
	c.DrawFillRect(w.X, w.Y, w.W, w.H, colWinBody)

	title := colWinTitle
	if d.focusWin == 0 {
		title = colWinTitleHi
	}
	c.DrawFillRect(w.X, w.Y, w.W, titleBarH, title)
	c.DrawString(w.X+8, w.Y+3, w.Title, colText)
	c.DrawRect(w.X, w.Y, w.W, w.H, colTopBarLine)

	tx, ty := w.X+16, w.Y+titleBarH+12
	banner := "Native Graphics (DRM/KMS)"
	if len(backend) > 0 && backend[0] != "" {
		banner = backend[0]
	}
	c.DrawString(tx, ty, banner, colText)

	s := append(d.scratch[:0], "Mouse   x="...)
	s = strconv.AppendInt(s, int64(st.X), 10)
	s = append(s, " y="...)
	s = strconv.AppendInt(s, int64(st.Y), 10)
	c.DrawBytes(tx, ty+22, s, colDim)

	s = append(s[:0], "Buttons "...)
	s = appendButtons(s, st.Buttons)
	c.DrawBytes(tx, ty+40, s, colDim)

	s = append(s[:0], "Frame   "...)
	s = strconv.AppendUint(s, d.frames, 10)
	s = append(s, "  ("...)
	s = strconv.AppendInt(s, int64(d.fps), 10)
	s = append(s, " fps)"...)
	c.DrawBytes(tx, ty+58, s, colDim)

	bx, by, bw, bh := d.buttonRect()
	bc := colButton
	if st.Buttons&input.Left != 0 && inside(st.X, st.Y, bx, by, bw, bh) {
		bc = colButtonDown
	}
	c.DrawFillRect(bx, by, bw, bh, bc)
	c.DrawRect(bx, by, bw, bh, colAccent)
	c.DrawString(bx+12, by+7, "Click me", colText)

	s = append(s[:0], "Clicks: "...)
	s = strconv.AppendUint(s, d.clicks, 10)
	c.DrawBytes(bx+bw+16, by+7, s, colText)

	// Graphical Syshud / System Monitor Panel
	px, py := w.X+190, w.Y+titleBarH+22
	c.DrawString(px, py, "SYS MONITOR", colAccent)

	// CPU Usage Bar
	c.DrawString(px, py+18, "CPU", colDim)
	c.DrawRect(px+36, py+18, 140, 12, colTopBarLine)
	cpuFill := int(d.cpuPercent * 138 / 100)
	if cpuFill > 138 {
		cpuFill = 138
	}
	if cpuFill > 0 {
		c.DrawFillRect(px+37, py+19, cpuFill, 10, colAccent)
	}

	// RAM Usage Bar
	c.DrawString(px, py+36, "RAM", colDim)
	c.DrawRect(px+36, py+36, 140, 12, colTopBarLine)
	ramFill := 0
	if d.memTotalMB > 0 {
		ramFill = int(d.memUsedMB * 138 / d.memTotalMB)
		if ramFill > 138 {
			ramFill = 138
		}
	}
	if ramFill > 0 {
		c.DrawFillRect(px+37, py+37, ramFill, 10, 0x2ECC71)
	}

	c.DrawString(tx, w.Y+w.H-24, "Drag windows. Ctrl-Q / Exit", colDim)
	d.scratch = s[:0]
}

func appendButtons(s []byte, b uint8) []byte {
	if b == 0 {
		return append(s, '-')
	}
	if b&input.Left != 0 {
		s = append(s, 'L')
	}
	if b&input.Middle != 0 {
		s = append(s, 'M')
	}
	if b&input.Right != 0 {
		s = append(s, 'R')
	}
	return s
}

// Run drives the 60 FPS loop. wait(d) blocks up to d and returns true when
// the user asked to quit. The mouse bounds are set to the screen size
// for the duration and restored afterwards.
func (d *Desktop) Run(dev Device, m *input.Mouse, wait func(time.Duration) bool) {
	defer func() {
		if d.Term != nil {
			_ = d.Term.Close()
		}
	}()

	if wait == nil {
		klist, _ := OpenDualKeyboard()
		if klist != nil {
			defer klist.Close()
		}
		wait = func(dur time.Duration) bool {
			deadline := time.Now().Add(dur)
			for {
				rem := time.Until(deadline)
				if rem <= 0 {
					return false
				}
				var data []byte
				var ok, eof bool
				if klist != nil {
					data, ok, eof = klist.Poll(rem)
				}
				if eof {
					return true
				}
				if !ok {
					return false
				}
				if d.RouteInput(data) {
					return true
				}
			}
		}
	}

	prev := m.Snapshot()
	m.SetBounds(dev.Width(), dev.Height())
	defer m.SetBounds(prev.W, prev.H)

	c := dev.Surface()
	cur := NewCursor()
	const frame = time.Second / 60
	next := time.Now()
	for {
		if d.shouldQuit {
			return
		}
		st := m.Snapshot()
		d.Update(st)
		if d.shouldQuit {
			return
		}
		d.Draw(c, st, dev.Backend())
		cur.Draw(c, st.X, st.Y)
		dev.Blit()
		cur.Restore(c)

		next = next.Add(frame)
		if now := time.Now(); next.Before(now) {
			next = now // fell behind: don't try to catch up
		}
		if wait(time.Until(next)) {
			return
		}
	}
}
