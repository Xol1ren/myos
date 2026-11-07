package fb

import (
	"net"
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

// Window is the movable demo window.
type Window struct {
	X, Y, W, H int
	Title      string
}

// Desktop is the demo desktop: background, top bar and one draggable window.
type Desktop struct {
	Win Window

	clicks   uint64
	frames   uint64
	fps      int
	fpsCount int
	fpsStamp time.Time

	prevBtn        uint8
	dragging       bool
	dragDX, dragDY int

	statusAt   time.Time
	barRight   []byte // "MEM a/b MB  IP x.x.x.x" refreshed once a second
	clock      []byte
	scratch    []byte
	memUsedMB  uint64
	memTotalMB uint64
	cpuPercent float64
	prevCPU    monitor.CPUTimes
}

// NewDesktop creates a desktop for a w*h screen.
func NewDesktop(w, h int) *Desktop {
	ww, wh := 400, 220
	return &Desktop{
		Win:      Window{X: max((w-ww)/2, 0), Y: max((h-wh)/2, topBarH), W: ww, H: wh, Title: "myos - pure Go desktop"},
		fpsStamp: time.Now(),
		scratch:  make([]byte, 0, 128),
	}
}

func (d *Desktop) buttonRect() (x, y, w, h int) {
	return d.Win.X + 16, d.Win.Y + titleBarH + 112, 140, 30
}

func inside(px, py, x, y, w, h int) bool { return px >= x && py >= y && px < x+w && py < y+h }

// Update handles mouse interaction (dragging, button clicks) for one frame.
func (d *Desktop) Update(st input.State) {
	pressed := st.Buttons&input.Left != 0 && d.prevBtn&input.Left == 0
	released := st.Buttons&input.Left == 0
	w := &d.Win
	if pressed {
		if inside(st.X, st.Y, w.X, w.Y, w.W, titleBarH) {
			d.dragging, d.dragDX, d.dragDY = true, st.X-w.X, st.Y-w.Y
		} else if bx, by, bw, bh := d.buttonRect(); inside(st.X, st.Y, bx, by, bw, bh) {
			d.clicks++
		}
	}
	if released {
		d.dragging = false
	}
	if d.dragging {
		w.X = min(max(st.X-d.dragDX, 0), max(st.W-w.W, 0))
		w.Y = min(max(st.Y-d.dragDY, topBarH), max(st.H-w.H, topBarH))
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

// Draw renders the whole scene (not the cursor) into c.
func (d *Desktop) Draw(c *Canvas, st input.State, backend ...string) {
	c.DrawFillRect(0, 0, c.W, c.H, colBackground)

	// Top bar: title on the left, clock centred, memory/IP on the right.
	c.DrawFillRect(0, 0, c.W, topBarH, colTopBar)
	c.DrawFillRect(0, topBarH-1, c.W, 1, colTopBarLine)
	c.DrawString(8, 4, "myos", colAccent)
	c.DrawBytes((c.W-len(d.clock)*GlyphW)/2, 4, d.clock, colText)
	c.DrawBytes(c.W-len(d.barRight)*GlyphW-8, 4, d.barRight, colDim)

	// Window with a subtle shadow.
	w := &d.Win
	c.DrawFillRect(w.X+5, w.Y+5, w.W, w.H, 0x060C1C)
	c.DrawFillRect(w.X, w.Y, w.W, w.H, colWinBody)
	title := colWinTitle
	if d.dragging {
		title = colWinTitleHi
	}
	c.DrawFillRect(w.X, w.Y, w.W, titleBarH, title)
	c.DrawString(w.X+8, w.Y+3, w.Title, colText)
	c.DrawRect(w.X, w.Y, w.W, w.H, colTopBarLine)

	tx, ty := w.X+16, w.Y+titleBarH+12
	banner := "Native Graphics (no X11/Wayland)"
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

	// Graphical Syshud / System Monitor Panel (Phase 4 integration)
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

	c.DrawString(tx, w.Y+w.H-24, "Drag title bar. Esc/q: exit", colDim)
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
// the user asked to quit (Esc/q). The mouse bounds are set to the screen size
// for the duration and restored afterwards.
func (d *Desktop) Run(dev Device, m *input.Mouse, wait func(time.Duration) bool) {
	prev := m.Snapshot()
	m.SetBounds(dev.Width(), dev.Height())
	defer m.SetBounds(prev.W, prev.H)

	c := dev.Surface()
	cur := NewCursor()
	const frame = time.Second / 60
	next := time.Now()
	for {
		st := m.Snapshot()
		d.Update(st)
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
