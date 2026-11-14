package fb

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
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
	topBarH       = 24
	titleBarH     = 22
	dockH         = 32
	maxTerminals  = 4
	winBtnSize    = 18
	winBtnPad     = 4
	cascadeOffset = 24
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

// WindowKind identifies the content type of a desktop window.
type WindowKind int

const (
	WinTerminal WindowKind = iota
	WinTelemetry
	WinFiles
)

// Window is a movable, focusable, minimizable desktop window.
type Window struct {
	ID         int
	Kind       WindowKind
	X, Y, W, H int
	Title      string
	Minimized  bool
	Term       *Terminal
	Files      *FileExplorer
}

type rect struct {
	x, y, w, h int
}

func (r rect) contains(px, py int) bool {
	return inside(px, py, r.x, r.y, r.w, r.h)
}

// Desktop is the multi-window desktop environment with topbar, Z-ordered windows, and bottom dock.
type Desktop struct {
	Windows []*Window

	width      int
	height     int
	focusedID  int
	draggingID int
	dragDX     int
	dragDY     int
	nextWinID  int
	termSeq    int
	shouldQuit bool

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

// NewDesktop creates a desktop with an initial Terminal window and Telemetry (System Monitor) window.
func NewDesktop(w, h int) *Desktop {
	if w <= 0 {
		w = 1280
	}
	if h <= 0 {
		h = 800
	}
	d := &Desktop{
		width:    w,
		height:   h,
		fpsStamp: time.Now(),
		scratch:  make([]byte, 0, 128),
	}

	// Spawn Telemetry (System Monitor) first, then Terminal on top.
	d.SpawnTelemetry()
	d.SpawnTerminal()
	return d
}

func (d *Desktop) terminalCount() int {
	n := 0
	for _, w := range d.Windows {
		if w.Kind == WinTerminal {
			n++
		}
	}
	return n
}

func (d *Desktop) defaultTermSize() (cols, rows, tw, th int) {
	cols, rows = 70, 20
	tw = cols*GlyphW + 16
	th = rows*GlyphH + titleBarH + 12
	if tw > d.width-40 {
		cols = max((d.width-56)/GlyphW, 40)
		tw = cols*GlyphW + 16
	}
	if th > d.height-topBarH-dockH-20 {
		rows = max((d.height-topBarH-dockH-44)/GlyphH, 10)
		th = rows*GlyphH + titleBarH + 12
	}
	return cols, rows, tw, th
}

// SpawnTerminal creates a new PTY terminal window (capped at 4 concurrent terminals)
// with cascading coordinates, brings it to front, and focuses it.
func (d *Desktop) SpawnTerminal() *Window {
	if d.terminalCount() >= maxTerminals {
		// Focus the topmost existing terminal if at cap.
		for i := len(d.Windows) - 1; i >= 0; i-- {
			if d.Windows[i].Kind == WinTerminal {
				d.FocusWindow(d.Windows[i].ID)
				return d.Windows[i]
			}
		}
		return nil
	}

	cols, rows, tw, th := d.defaultTermSize()
	d.termSeq++
	offset := ((d.termSeq - 1) % 4) * cascadeOffset
	tx := min(60+offset, max(d.width-tw-10, 10))
	ty := min(topBarH+24+offset, max(d.height-dockH-th-10, topBarH+4))

	term, err := NewTerminal(rows, cols)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[desktop] terminal init: %v\n", err)
		return nil
	}

	d.nextWinID++
	title := fmt.Sprintf("Terminal %d", d.termSeq)
	if term.title != "" && d.termSeq == 1 {
		title = term.title
	}
	win := &Window{
		ID:    d.nextWinID,
		Kind:  WinTerminal,
		X:     tx,
		Y:     ty,
		W:     tw,
		H:     th,
		Title: title,
		Term:  term,
	}
	d.Windows = append(d.Windows, win)
	d.focusedID = win.ID
	return win
}

// SpawnFiles opens the graphical File Explorer window, or restores and focuses it if already open.
func (d *Desktop) SpawnFiles() *Window {
	for _, w := range d.Windows {
		if w.Kind == WinFiles {
			d.FocusWindow(w.ID)
			return w
		}
	}
	fw := min(500, max(d.width-40, 320))
	fh := min(380, max(d.height-topBarH-dockH-40, 220))
	fx := max((d.width-fw)/2, 20)
	fy := max(topBarH+40, 20)
	d.nextWinID++
	win := &Window{
		ID:    d.nextWinID,
		Kind:  WinFiles,
		X:     fx,
		Y:     fy,
		W:     fw,
		H:     fh,
		Title: "File Explorer",
		Files: NewFileExplorer("/"),
	}
	d.Windows = append(d.Windows, win)
	d.focusedID = win.ID
	return win
}

// SpawnTelemetry opens the System Monitor / Telemetry window, or restores and focuses it if already open.
func (d *Desktop) SpawnTelemetry() *Window {
	for _, w := range d.Windows {
		if w.Kind == WinTelemetry {
			d.FocusWindow(w.ID)
			return w
		}
	}
	sysW, sysH := 420, 240
	_, _, tw, _ := d.defaultTermSize()
	sysX := 60 + tw + 20
	sysY := topBarH + 24
	if sysX+sysW > d.width-10 {
		sysX = max(d.width-sysW-20, 20)
		sysY = max(d.height-dockH-sysH-20, topBarH+10)
	}
	d.nextWinID++
	win := &Window{
		ID:    d.nextWinID,
		Kind:  WinTelemetry,
		X:     sysX,
		Y:     sysY,
		W:     sysW,
		H:     sysH,
		Title: "System Monitor",
	}
	d.Windows = append(d.Windows, win)
	d.focusedID = win.ID
	return win
}

// FocusWindow unminimizes the window with id, moves it to the top of the Z-order, and focuses it.
func (d *Desktop) FocusWindow(id int) {
	idx := -1
	for i, w := range d.Windows {
		if w.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	w := d.Windows[idx]
	w.Minimized = false
	if idx != len(d.Windows)-1 {
		copy(d.Windows[idx:], d.Windows[idx+1:])
		d.Windows[len(d.Windows)-1] = w
	}
	d.focusedID = w.ID
}

// MinimizeWindow hides the window with id and moves focus to the next topmost visible window.
func (d *Desktop) MinimizeWindow(id int) {
	for _, w := range d.Windows {
		if w.ID == id {
			w.Minimized = true
			break
		}
	}
	if d.draggingID == id {
		d.draggingID = 0
	}
	if d.focusedID == id {
		d.focusedID = 0
		for i := len(d.Windows) - 1; i >= 0; i-- {
			if !d.Windows[i].Minimized {
				d.focusedID = d.Windows[i].ID
				break
			}
		}
	}
}

// CloseWindow removes the window with id and releases any associated PTY/child resources.
func (d *Desktop) CloseWindow(id int) {
	for i, w := range d.Windows {
		if w.ID == id {
			if w.Term != nil {
				_ = w.Term.Close()
				w.Term = nil
			}
			d.Windows = append(d.Windows[:i], d.Windows[i+1:]...)
			break
		}
	}
	if d.draggingID == id {
		d.draggingID = 0
	}
	if d.focusedID == id {
		d.focusedID = 0
		for i := len(d.Windows) - 1; i >= 0; i-- {
			if !d.Windows[i].Minimized {
				d.focusedID = d.Windows[i].ID
				break
			}
		}
	}
}

func (d *Desktop) focusedWindow() *Window {
	for i := len(d.Windows) - 1; i >= 0; i-- {
		w := d.Windows[i]
		if !w.Minimized && w.ID == d.focusedID {
			return w
		}
	}
	return nil
}

func (d *Desktop) findWindowByKind(kind WindowKind) *Window {
	for i := len(d.Windows) - 1; i >= 0; i-- {
		if d.Windows[i].Kind == kind {
			return d.Windows[i]
		}
	}
	return nil
}

func (d *Desktop) telemetryButtonRect(w *Window) (x, y, bw, bh int) {
	return w.X + 16, w.Y + titleBarH + 112, 140, 30
}

func winMinButtonRect(w *Window) rect {
	return rect{w.X + w.W - 2*winBtnSize - 2*winBtnPad - 2, w.Y + 2, winBtnSize, winBtnSize}
}

func winCloseButtonRect(w *Window) rect {
	return rect{w.X + w.W - winBtnSize - winBtnPad, w.Y + 2, winBtnSize, winBtnSize}
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

// dockLayout computes the rectangles for the bottom dock launcher buttons and task buttons.
type dockItem struct {
	rect  rect
	label string
	kind  int // 0=launcher-term, 1=launcher-files, 2=launcher-telemetry, 3=window-task
	win   *Window
}

func (d *Desktop) dockItems(screenW, screenH int) []dockItem {
	if screenW <= 0 {
		screenW = d.width
	}
	if screenH <= 0 {
		screenH = d.height
	}
	y := screenH - dockH + 4
	bh := dockH - 8
	x := 8

	items := make([]dockItem, 0, 3+len(d.Windows))
	launchers := []struct {
		label string
		kind  int
	}{
		{"+ Terminal", 0},
		{"Files", 1},
		{"Telemetry", 2},
	}
	for _, l := range launchers {
		bw := len(l.label)*GlyphW + 16
		items = append(items, dockItem{
			rect:  rect{x, y, bw, bh},
			label: l.label,
			kind:  l.kind,
		})
		x += bw + 6
	}

	x += 8 // separator gap

	// Sort windows by ID for stable taskbar ordering.
	sorted := make([]*Window, len(d.Windows))
	copy(sorted, d.Windows)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	for _, w := range sorted {
		lbl := w.Title
		if len(lbl) > 14 {
			lbl = lbl[:14]
		}
		bw := len(lbl)*GlyphW + 16
		if x+bw > screenW-8 {
			break
		}
		items = append(items, dockItem{
			rect:  rect{x, y, bw, bh},
			label: lbl,
			kind:  3,
			win:   w,
		})
		x += bw + 6
	}
	return items
}

// RouteInput routes keyboard bytes to the focused terminal or handles quit shortcuts.
func (d *Desktop) RouteInput(data []byte) bool {
	for _, b := range data {
		if b == 0x11 { // Ctrl-Q exits desktop
			d.shouldQuit = true
			return true
		}
	}
	if fw := d.focusedWindow(); fw != nil && fw.Kind == WinTerminal && fw.Term != nil && fw.Term.IsActive() {
		_, _ = fw.Term.WriteInput(data)
		return false
	}
	// Fallback when no terminal is focused
	for _, b := range data {
		if b == 'q' || b == 'Q' || b == 27 || b == 3 {
			d.shouldQuit = true
			return true
		}
	}
	return false
}

// RouteKey routes keyboard input from the GUI loop to the focused terminal or handles quit shortcuts.
func (d *Desktop) RouteKey(k byte) bool {
	return d.RouteInput([]byte{k})
}

// Update handles mouse interaction (dock, Z-order focus, window dragging, minimize/close, buttons) for one frame.
func (d *Desktop) Update(st input.State) {
	pressed := st.Buttons&input.Left != 0 && d.prevBtn&input.Left == 0
	released := st.Buttons&input.Left == 0

	sw := st.W
	if sw <= 0 || (d.width > 0 && sw != d.width) {
		sw = d.width
	}
	sh := st.H
	if sh <= 0 || (d.height > 0 && sh != d.height) {
		sh = d.height
	}

	if pressed {
		// 1. Topbar Exit Button
		ex, ey, ew, eh := d.exitButtonRect(sw)
		if inside(st.X, st.Y, ex, ey, ew, eh) {
			d.shouldQuit = true
			return
		}

		// 2. Bottom Dock / Taskbar
		if st.Y >= sh-dockH {
			for _, item := range d.dockItems(sw, sh) {
				if item.rect.contains(st.X, st.Y) {
					switch item.kind {
					case 0:
						d.SpawnTerminal()
					case 1:
						d.SpawnFiles()
					case 2:
						d.SpawnTelemetry()
					case 3:
						if item.win != nil {
							d.FocusWindow(item.win.ID)
						}
					}
					break
				}
			}
		} else {
			// 3. Z-ordered Windows (iterate top-to-bottom)
			for i := len(d.Windows) - 1; i >= 0; i-- {
				w := d.Windows[i]
				if w.Minimized || !inside(st.X, st.Y, w.X, w.Y, w.W, w.H) {
					continue
				}
				if winCloseButtonRect(w).contains(st.X, st.Y) {
					d.CloseWindow(w.ID)
					break
				}
				if winMinButtonRect(w).contains(st.X, st.Y) {
					d.MinimizeWindow(w.ID)
					break
				}
				d.FocusWindow(w.ID)
				if inside(st.X, st.Y, w.X, w.Y, w.W, titleBarH) {
					d.draggingID = w.ID
					d.dragDX, d.dragDY = st.X-w.X, st.Y-w.Y
				} else if w.Kind == WinTelemetry {
					if bx, by, bw, bh := d.telemetryButtonRect(w); inside(st.X, st.Y, bx, by, bw, bh) {
						d.clicks++
					}
				} else if w.Kind == WinFiles && w.Files != nil {
					if editPath := w.Files.HandleClick(st.X, st.Y, w); editPath != "" {
						if tw := d.SpawnTerminal(); tw != nil && tw.Term != nil {
							quoted := "'" + strings.ReplaceAll(editPath, "'", "'\\''") + "'"
							_, _ = tw.Term.WriteInput([]byte("edit " + quoted + "\n"))
						}
					}
				}
				break
			}
		}
	}

	if released {
		d.draggingID = 0
	}

	// Update dragged window position
	if d.draggingID != 0 {
		for _, w := range d.Windows {
			if w.ID == d.draggingID {
				w.X = min(max(st.X-d.dragDX, 0), max(sw-w.W, 0))
				w.Y = min(max(st.Y-d.dragDY, topBarH), max(sh-dockH-w.H, topBarH))
				break
			}
		}
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

	// Top bar
	c.DrawFillRect(0, 0, c.W, topBarH, colTopBar)
	c.DrawFillRect(0, topBarH-1, c.W, 1, colTopBarLine)
	c.DrawString(8, 4, "myos", colAccent)
	c.DrawBytes((c.W-len(d.clock)*GlyphW)/2, 4, d.clock, colText)
	c.DrawBytes(c.W-len(d.barRight)*GlyphW-76, 4, d.barRight, colDim)

	ex, ey, ew, eh := d.exitButtonRect(c.W)
	c.DrawFillRect(ex, ey, ew, eh, colButton)
	c.DrawRect(ex, ey, ew, eh, colAccent)
	c.DrawString(ex+14, ey+2, "Exit", colText)

	// Z-ordered windows (back-to-front)
	for _, w := range d.Windows {
		if w.Minimized {
			continue
		}
		focused := w.ID == d.focusedID
		switch w.Kind {
		case WinTerminal:
			if w.Term != nil {
				w.Term.Draw(c, w.X, w.Y, w.W, w.H, focused)
				d.drawWindowTitleButtons(c, w, focused)
			}
		case WinTelemetry:
			d.drawTelemetry(c, w, focused, st, backend...)
		case WinFiles:
			d.drawWindowChrome(c, w, focused)
			if w.Files != nil {
				w.Files.Draw(c, w)
			}
		}
	}

	// Bottom Dock / Taskbar
	d.drawDock(c)
}

func (d *Desktop) drawWindowTitleButtons(c *Canvas, w *Window, focused bool) {
	borderCol := colTopBarLine
	if focused {
		borderCol = colAccent
	}
	c.DrawRect(w.X, w.Y, w.W, w.H, borderCol)

	mb := winMinButtonRect(w)
	cb := winCloseButtonRect(w)
	titleBg := colWinTitle
	if focused {
		titleBg = colWinTitleHi
	}
	// Cover any prior titlebar right-side text cleanly before drawing [-] and [x].
	c.DrawFillRect(mb.x-2, w.Y+1, w.X+w.W-mb.x+1, titleBarH-2, titleBg)

	c.DrawFillRect(mb.x, mb.y, mb.w, mb.h, colButton)
	c.DrawRect(mb.x, mb.y, mb.w, mb.h, colTopBarLine)
	c.DrawString(mb.x+5, mb.y+1, "-", colText)

	c.DrawFillRect(cb.x, cb.y, cb.w, cb.h, 0x9E2A2B)
	c.DrawRect(cb.x, cb.y, cb.w, cb.h, colTopBarLine)
	c.DrawString(cb.x+5, cb.y+1, "x", colText)
}

func (d *Desktop) drawWindowChrome(c *Canvas, w *Window, focused bool) {
	c.DrawFillRect(w.X+5, w.Y+5, w.W, w.H, 0x060C1C)
	c.DrawFillRect(w.X, w.Y, w.W, w.H, colWinBody)

	title := colWinTitle
	if focused {
		title = colWinTitleHi
	}
	c.DrawFillRect(w.X, w.Y, w.W, titleBarH, title)
	c.DrawString(w.X+8, w.Y+3, w.Title, colText)
	d.drawWindowTitleButtons(c, w, focused)
}

func (d *Desktop) drawTelemetry(c *Canvas, w *Window, focused bool, st input.State, backend ...string) {
	d.drawWindowChrome(c, w, focused)

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

	bx, by, bw, bh := d.telemetryButtonRect(w)
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

	px, py := w.X+190, w.Y+titleBarH+22
	c.DrawString(px, py, "SYS MONITOR", colAccent)

	c.DrawString(px, py+18, "CPU", colDim)
	c.DrawRect(px+36, py+18, 140, 12, colTopBarLine)
	cpuFill := min(int(d.cpuPercent*138/100), 138)
	if cpuFill > 0 {
		c.DrawFillRect(px+37, py+19, cpuFill, 10, colAccent)
	}

	c.DrawString(px, py+36, "RAM", colDim)
	c.DrawRect(px+36, py+36, 140, 12, colTopBarLine)
	ramFill := 0
	if d.memTotalMB > 0 {
		ramFill = min(int(d.memUsedMB*138/d.memTotalMB), 138)
	}
	if ramFill > 0 {
		c.DrawFillRect(px+37, py+37, ramFill, 10, 0x2ECC71)
	}

	c.DrawString(tx, w.Y+w.H-24, "Drag windows. Ctrl-Q / Exit", colDim)
	d.scratch = s[:0]
}

func (d *Desktop) drawDock(c *Canvas) {
	y := c.H - dockH
	c.DrawFillRect(0, y, c.W, dockH, colTopBar)
	c.DrawFillRect(0, y, c.W, 1, colTopBarLine)

	for _, item := range d.dockItems(c.W, c.H) {
		bg := colButtonDown
		border := colTopBarLine
		fg := colText
		if item.kind < 3 {
			bg = colButton
			border = colAccent
		} else if item.win != nil {
			if item.win.Minimized {
				bg = colWinBody
				fg = colDim
			} else if item.win.ID == d.focusedID {
				bg = colWinTitle
				border = colAccent
			}
		}
		c.DrawFillRect(item.rect.x, item.rect.y, item.rect.w, item.rect.h, bg)
		c.DrawRect(item.rect.x, item.rect.y, item.rect.w, item.rect.h, border)
		c.DrawString(item.rect.x+8, item.rect.y+4, item.label, fg)
	}
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
		for _, w := range d.Windows {
			if w.Term != nil {
				_ = w.Term.Close()
				w.Term = nil
			}
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
