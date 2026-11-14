package fb

import (
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"myos/internal/input"
)

func TestCanvasPrimitivesClip(t *testing.T) {
	c := NewCanvas(8, 8)
	c.DrawPixel(-1, 0, 1)
	c.DrawPixel(8, 8, 1)
	c.DrawFillRect(-4, -4, 8, 8, 7) // only the 4x4 corner is visible
	n := 0
	for _, p := range c.Pix {
		if p == 7 {
			n++
		}
	}
	if n != 16 {
		t.Fatalf("filled %d pixels, want 16", n)
	}
	c.DrawRect(0, 0, 8, 8, 9)
	if c.Pix[7*8+7] != 9 || c.Pix[3*8+3] != 7 {
		t.Fatal("outline wrong")
	}
}

func TestDrawGlyph(t *testing.T) {
	c := NewCanvas(16, 16)
	c.DrawGlyph(0, 0, 'A', 1)
	if c.Pix[2*16+3] != 1 { // apex of 'A' in default8x16
		t.Fatal("apex pixel missing")
	}
}

func TestCursorSaveUnder(t *testing.T) {
	c := NewCanvas(40, 40)
	for i := range c.Pix {
		c.Pix[i] = uint32(i) + 100
	}
	orig := append([]uint32(nil), c.Pix...)
	cu := NewCursor()
	for _, pos := range [][2]int{{5, 5}, {-3, -3}, {35, 35}, {10, 12}} {
		cu.Draw(&c, pos[0], pos[1])
		changed := false
		for i := range c.Pix {
			changed = changed || c.Pix[i] != orig[i]
		}
		if !changed && pos[0] > 0 && pos[0] < 30 {
			t.Fatal("cursor not drawn")
		}
		cu.Restore(&c)
		for i := range c.Pix {
			if c.Pix[i] != orig[i] {
				t.Fatalf("pixel %d corrupted after restore at %v", i, pos)
			}
		}
	}
}

func fakeFB(t *testing.T, bpp, w, h, stride int, r, g, b bitfield) *Framebuffer {
	t.Helper()
	fbuf := &Framebuffer{mem: make([]byte, stride*h)}
	vi := fbVarScreenInfo{xres: uint32(w), yres: uint32(h), bitsPerPixel: uint32(bpp), red: r, green: g, blue: b}
	if err := fbuf.init(vi, fbFixScreenInfo{lineLength: uint32(stride)}); err != nil {
		t.Fatal(err)
	}
	return fbuf
}

func TestBlit32Fast(t *testing.T) {
	f := fakeFB(t, 32, 4, 2, 20, bitfield{16, 8, 0}, bitfield{8, 8, 0}, bitfield{0, 8, 0})
	if !f.fast32 {
		t.Fatal("expected fast path")
	}
	f.DrawPixel(1, 1, 0x112233)
	f.Blit()
	off := 20 + 4 // row 1 + x=1
	if got := uint32(f.mem[off]) | uint32(f.mem[off+1])<<8 | uint32(f.mem[off+2])<<16; got != 0x112233 {
		t.Fatalf("got %#x", got)
	}
}

func TestBlit16Convert(t *testing.T) {
	f := fakeFB(t, 16, 2, 1, 4, bitfield{11, 5, 0}, bitfield{5, 6, 0}, bitfield{0, 5, 0})
	f.DrawPixel(0, 0, 0xFF0000)
	f.DrawPixel(1, 0, 0x0000FF)
	f.Blit()
	if v := uint16(f.mem[0]) | uint16(f.mem[1])<<8; v != 0xF800 {
		t.Fatalf("red=%#x", v)
	}
	if v := uint16(f.mem[2]) | uint16(f.mem[3])<<8; v != 0x001F {
		t.Fatalf("blue=%#x", v)
	}
}

func TestBlitNoAlloc(t *testing.T) {
	f := fakeFB(t, 32, 64, 64, 256, bitfield{16, 8, 0}, bitfield{8, 8, 0}, bitfield{0, 8, 0})
	if n := testing.AllocsPerRun(20, f.Blit); n != 0 {
		t.Fatalf("Blit allocates %v times", n)
	}
	g := fakeFB(t, 16, 64, 64, 128, bitfield{11, 5, 0}, bitfield{5, 6, 0}, bitfield{0, 5, 0})
	if n := testing.AllocsPerRun(20, g.Blit); n != 0 {
		t.Fatalf("slow-path Blit allocates %v times", n)
	}
}

func TestDesktopInteraction(t *testing.T) {
	d := NewDesktop(640, 480)
	c := NewCanvas(640, 480)
	st := input.State{X: 100, Y: 100, W: 640, H: 480}
	d.Update(st)
	d.Draw(&c, st)

	// Click the button inside Telemetry window
	telWin := d.findWindowByKind(WinTelemetry)
	if telWin == nil {
		t.Fatal("missing telemetry window")
	}
	d.FocusWindow(telWin.ID)
	bx, by, _, _ := d.telemetryButtonRect(telWin)
	clickSt := input.State{X: bx + 5, Y: by + 5, Buttons: input.Left, W: 640, H: 480}
	d.Update(clickSt)
	if d.clicks != 1 {
		t.Fatalf("expected 1 click, got %d", d.clicks)
	}

	// Release click
	relSt := input.State{X: bx + 5, Y: by + 5, Buttons: 0, W: 640, H: 480}
	d.Update(relSt)
	if d.clicks != 1 {
		t.Fatalf("expected 1 click after release, got %d", d.clicks)
	}
}

func TestDRMUAPIBinaryLayout(t *testing.T) {
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"drmCardRes", unsafe.Sizeof(drmCardRes{}), 64},
		{"drmModeInfo", unsafe.Sizeof(drmModeInfo{}), 68},
		{"drmCRTC", unsafe.Sizeof(drmCRTC{}), 104},
		{"drmGetConnector", unsafe.Sizeof(drmGetConnector{}), 80},
		{"drmGetEncoder", unsafe.Sizeof(drmGetEncoder{}), 20},
		{"drmCreateDumb", unsafe.Sizeof(drmCreateDumb{}), 32},
		{"drmMapDumb", unsafe.Sizeof(drmMapDumb{}), 16},
		{"drmDestroyDumb", unsafe.Sizeof(drmDestroyDumb{}), 4},
		{"drmFBCmd", unsafe.Sizeof(drmFBCmd{}), 28},
		{"drmFBCmd2", unsafe.Sizeof(drmFBCmd2{}), 104},
		{"drmFBDirtyCmd", unsafe.Sizeof(drmFBDirtyCmd{}), 24},
		{"drmGetCap", unsafe.Sizeof(drmGetCap{}), 16},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("sizeof(%s) = %d, want %d", tc.name, tc.got, tc.want)
		}
	}

	// Verify key offset alignments
	var fb2 drmFBCmd2
	if off := unsafe.Offsetof(fb2.modifier); off != 72 {
		t.Errorf("offsetof(drmFBCmd2.modifier) = %d, want 72", off)
	}
	var crtc drmCRTC
	if off := unsafe.Offsetof(crtc.mode); off != 36 {
		t.Errorf("offsetof(drmCRTC.mode) = %d, want 36", off)
	}
}

func TestModeSelection(t *testing.T) {
	// Empty slice
	if m := selectBestMode(nil); m.HDisplay != 0 {
		t.Fatalf("expected empty mode, got %+v", m)
	}

	// No preferred flag -> picks first mode
	m1 := drmModeInfo{HDisplay: 800, VDisplay: 600}
	m2 := drmModeInfo{HDisplay: 1024, VDisplay: 768}
	if m := selectBestMode([]drmModeInfo{m1, m2}); m.HDisplay != 800 {
		t.Fatalf("expected first mode 800x600, got %dx%d", m.HDisplay, m.VDisplay)
	}

	// Preferred flag present -> picks preferred
	m3 := drmModeInfo{HDisplay: 1280, VDisplay: 800, Type: drmModeTypePreferred}
	if m := selectBestMode([]drmModeInfo{m1, m2, m3}); m.HDisplay != 1280 {
		t.Fatalf("expected preferred mode 1280x800, got %dx%d", m.HDisplay, m.VDisplay)
	}
}

func TestCRTCSelection(t *testing.T) {
	crtcs := []uint32{10, 20, 30}
	// Empty CRTC list
	if id := selectCRTC(0, drmGetConnector{}, nil); id != 0 {
		t.Fatalf("expected 0 for empty CRTCs, got %d", id)
	}
	// Default to first CRTC when no encoder bound, without mutating caller's connector
	conn := drmGetConnector{countModes: 3}
	if id := selectCRTC(0, conn, crtcs); id != 10 {
		t.Fatalf("expected default CRTC 10, got %d", id)
	}
	if conn.countModes != 3 {
		t.Fatalf("selectCRTC mutated caller's connector countModes: %d", conn.countModes)
	}
}

type mockDevice struct {
	Canvas
	blits  int
	closed bool
}

func (m *mockDevice) Width() int       { return m.Canvas.W }
func (m *mockDevice) Height() int      { return m.Canvas.H }
func (m *mockDevice) Surface() *Canvas { return &m.Canvas }
func (m *mockDevice) Blit()            { m.blits++ }
func (m *mockDevice) Close() error     { m.closed = true; return nil }
func (m *mockDevice) Backend() string  { return "MockDevice (100x100)" }

func TestDeviceInterface(t *testing.T) {
	// Ensure *Framebuffer and *DRMDevice implement Device
	var _ Device = (*Framebuffer)(nil)
	var _ Device = (*DRMDevice)(nil)

	mock := &mockDevice{Canvas: NewCanvas(100, 100)}
	var dev Device = mock
	if dev.Width() != 100 || dev.Height() != 100 {
		t.Fatalf("unexpected dimensions: %dx%d", dev.Width(), dev.Height())
	}
	if dev.Backend() != "MockDevice (100x100)" {
		t.Fatalf("unexpected backend: %s", dev.Backend())
	}

	// Run desktop with mock device for 1 frame
	d := NewDesktop(100, 100)
	mouse := input.New("/dev/null", 100, 100)
	ran := false
	d.Run(dev, mouse, func(time.Duration) bool {
		ran = true
		return true // quit immediately
	})
	if !ran || mock.blits == 0 {
		t.Fatalf("Desktop.Run did not blit to device (blits=%d)", mock.blits)
	}
}

func TestOpenDeviceRouting(t *testing.T) {
	if _, err := OpenDevice("/nonexistent/card99"); err == nil {
		t.Fatal("expected error opening nonexistent card")
	}
	if _, err := OpenDevice("/nonexistent/fb99"); err == nil {
		t.Fatal("expected error opening nonexistent fb")
	}
	_, err := OpenDevice("")
	if err == nil {
		// If running on a system with a live graphics card, that's fine
		return
	}
	// Error should mention both DRM and Framebuffer
	errMsg := err.Error()
	if !strings.Contains(errMsg, "DRM") || !strings.Contains(errMsg, "Framebuffer") {
		t.Fatalf("diagnostic error should mention DRM and Framebuffer: %s", errMsg)
	}
}

func TestDesktopDrawWithBackendAndSyshud(t *testing.T) {
	d := NewDesktop(800, 600)
	c := NewCanvas(800, 600)
	st := input.State{X: 200, Y: 150, W: 800, H: 600}
	d.cpuPercent = 42.5
	d.memUsedMB = 128
	d.memTotalMB = 512
	d.Update(st)
	d.Draw(&c, st, "DRM/KMS (card0 @ 1024x768)")

	// Verify top bar and background pixels are painted
	if c.Pix[0] != colTopBar {
		t.Fatalf("top bar pixel wrong: %#x, want %#x", c.Pix[0], colTopBar)
	}
	if c.Pix[50*800+50] != colBackground {
		t.Fatalf("background pixel wrong: %#x, want %#x", c.Pix[50*800+50], colBackground)
	}
}

func TestDesktopMultipleCycles(t *testing.T) {
	d := NewDesktop(800, 600)
	mouse := input.New("/dev/null", 800, 600)

	for cycle := 1; cycle <= 3; cycle++ {
		mock := &mockDevice{Canvas: NewCanvas(800, 600)}
		frameCount := 0
		d.Run(mock, mouse, func(time.Duration) bool {
			frameCount++
			return frameCount >= 20 // 20 frames per cycle
		})
		if frameCount < 20 {
			t.Fatalf("cycle %d ran %d frames, want 20", cycle, frameCount)
		}
		if mock.blits < 20 {
			t.Fatalf("cycle %d blits %d, want >= 20", cycle, mock.blits)
		}
		if err := mock.Close(); err != nil {
			t.Fatalf("cycle %d close error: %v", cycle, err)
		}
		if !mock.closed {
			t.Fatalf("cycle %d device not closed", cycle)
		}
	}
}

func TestDRMDeviceCloseSafety(t *testing.T) {
	// A nil or zero-value DRMDevice must be safe to close without panic
	dev := &DRMDevice{}
	if err := dev.Close(); err != nil {
		t.Fatalf("unexpected error closing empty DRMDevice: %v", err)
	}
	// Calling close a second time must also be safe
	if err := dev.Close(); err != nil {
		t.Fatalf("unexpected error on second close: %v", err)
	}
}

func TestBlitPerformance(t *testing.T) {
	w, h := 1280, 800
	stride := w * 4
	f := &DRMDevice{
		Canvas:   NewCanvas(w, h),
		mem:      make([]byte, stride*h),
		hw32:     make([]uint32, (stride*h)/4),
		stride32: stride / 4,
	}
	for i := range f.Canvas.Pix {
		f.Canvas.Pix[i] = uint32(i)
	}

	iterations := 200
	start := time.Now()
	for i := 0; i < iterations; i++ {
		f.Blit()
	}
	elapsed := time.Since(start)
	avgBlit := elapsed / time.Duration(iterations)
	t.Logf("1280x800 Blit Performance: %d blits in %v (avg: %v per blit, max theoretical: %.1f FPS)",
		iterations, elapsed, avgBlit, float64(time.Second)/float64(avgBlit))

	if avgBlit > 10*time.Millisecond {
		t.Fatalf("Blit is too slow: %v per frame (target < 5ms for 60 FPS)", avgBlit)
	}
}

func TestCursorBoundaryClippingAndEdges(t *testing.T) {
	c := NewCanvas(1280, 800)
	for i := range c.Pix {
		c.Pix[i] = 0x123456
	}
	orig := append([]uint32(nil), c.Pix...)

	cu := NewCursor()
	edgePositions := [][2]int{
		{0, 0},
		{-5, -5},
		{-curW, 0},
		{0, -curH},
		{1280 - curW, 800 - curH},
		{1280 - 1, 800 - 1},
		{1280 - 5, 800 - 5},
		{1280, 800},
		{1300, 850},
		{-20, 400},
		{1280 + 10, 400},
		{640, -15},
		{640, 800 + 10},
	}

	for _, pos := range edgePositions {
		// Draw and ensure no panics
		cu.Draw(&c, pos[0], pos[1])
		// Restore and ensure exact pixel restoration
		cu.Restore(&c)
		for i := range c.Pix {
			if c.Pix[i] != orig[i] {
				t.Fatalf("pixel %d modified after Restore at position %v", i, pos)
			}
		}
	}

	// Test DrawCursor and RestoreCursor on Canvas
	c.DrawCursor(cu, 1275, 795)
	c.RestoreCursor(cu)
	for i := range c.Pix {
		if c.Pix[i] != orig[i] {
			t.Fatalf("pixel %d modified after Canvas.RestoreCursor", i)
		}
	}
}

func TestCursorNilAndCanvasSafety(t *testing.T) {
	var nilCu *Cursor
	var nilCanvas *Canvas
	emptyCanvas := &Canvas{}

	// None of these should panic
	nilCu.Draw(nilCanvas, 10, 10)
	nilCu.Restore(nilCanvas)
	nilCanvas.DrawCursor(nilCu, 10, 10)
	nilCanvas.RestoreCursor(nilCu)
	nilCanvas.DrawPixel(5, 5, 0xFF)
	nilCanvas.DrawFillRect(0, 0, 10, 10, 0xFF)
	nilCanvas.DrawGlyph(0, 0, 'A', 0xFF)

	cu := NewCursor()
	cu.Draw(nilCanvas, 0, 0)
	cu.Restore(nilCanvas)
	cu.Draw(emptyCanvas, 0, 0)
	cu.Restore(emptyCanvas)
}

func TestDesktopExitButtonHitTest(t *testing.T) {
	d := NewDesktop(1280, 800)
	ex, ey, ew, eh := d.ExitButtonRect(1280)
	if ex != 1280-68 || ey != 2 || ew != 60 || eh != 20 {
		t.Fatalf("unexpected exit button rect: got (%d, %d, %d, %d), want (1212, 2, 60, 20)", ex, ey, ew, eh)
	}

	// Click outside button
	st := input.State{X: 100, Y: 10, W: 1280, H: 800, Buttons: input.Left}
	d.Update(st)
	if d.shouldQuit {
		t.Fatal("desktop should not quit when clicking outside [Exit]")
	}

	// Release
	st.Buttons = 0
	d.Update(st)

	// Click inside [Exit] button at top-left edge
	st.X, st.Y, st.Buttons = ex, ey, input.Left
	d.Update(st)
	if !d.shouldQuit {
		t.Fatal("desktop should quit when clicking top-left of [Exit] button")
	}

	// Reset and click inside [Exit] button at bottom-right edge
	d.shouldQuit = false
	st.Buttons = 0
	d.Update(st)
	st.X, st.Y, st.Buttons = ex+ew-1, ey+eh-1, input.Left
	d.Update(st)
	if !d.shouldQuit {
		t.Fatal("desktop should quit when clicking bottom-right of [Exit] button")
	}
}

func TestKeyboardListenerMultiplexing(t *testing.T) {
	p1r, p1w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer p1r.Close()
	defer p1w.Close()

	p2r, p2w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer p2r.Close()
	defer p2w.Close()

	l := &KeyboardListener{
		primaryFd: int(p1r.Fd()),
		ttyFd:     int(p2r.Fd()),
	}
	defer l.Close()

	// Write to primary (serial/host stdin)
	if _, err := p1w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf, ok, eof := l.Poll(50 * time.Millisecond)
	if !ok || eof || string(buf) != "hello" {
		t.Fatalf("primary poll failed: got ok=%v, eof=%v, buf=%q", ok, eof, string(buf))
	}

	// Write to secondary (/dev/tty0 QEMU console focus)
	if _, err := p2w.Write([]byte{0x1b, '[', 'A'}); err != nil {
		t.Fatal(err)
	}
	buf, ok, eof = l.Poll(50 * time.Millisecond)
	if !ok || eof || len(buf) != 3 || string(buf) != "\x1b[A" {
		t.Fatalf("secondary poll failed: got ok=%v, eof=%v, buf=%v", ok, eof, buf)
	}
}

func TestRouteInputTerminalRouting(t *testing.T) {
	d := NewDesktop(1280, 800)
	termWin := d.findWindowByKind(WinTerminal)
	if termWin == nil || termWin.Term == nil {
		t.Skip("skipping terminal test on platform without PTY support")
	}
	defer func() {
		for _, w := range d.Windows {
			if w.Term != nil {
				_ = w.Term.Close()
			}
		}
	}()

	// Test routing control keys & runes
	d.FocusWindow(termWin.ID)
	quit := d.RouteInput([]byte("echo hi\r\n"))
	if quit {
		t.Fatal("RouteInput should not quit for standard input")
	}

	// Test Backspace
	quit = d.RouteInput([]byte{0x7F, 0x08})
	if quit {
		t.Fatal("RouteInput should not quit on backspace")
	}

	// Test Arrow keys escape sequence
	quit = d.RouteInput([]byte{0x1b, '[', 'A'})
	if quit {
		t.Fatal("RouteInput should not quit on arrow escape sequence")
	}

	// Test Ctrl-Q exits
	quit = d.RouteInput([]byte{0x11})
	if !quit || !d.shouldQuit {
		t.Fatal("RouteInput should quit on Ctrl-Q (0x11)")
	}
}

func TestHideCursor(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	restore := HideCursor(int(w.Fd()))
	var buf [16]byte
	n, err := r.Read(buf[:])
	if err != nil || string(buf[:n]) != "\x1b[?25l" {
		t.Fatalf("expected \\x1b[?25l, got n=%d, err=%v, buf=%q", n, err, string(buf[:n]))
	}

	restore()
	n, err = r.Read(buf[:])
	if err != nil || string(buf[:n]) != "\x1b[?25h" {
		t.Fatalf("expected \\x1b[?25h, got n=%d, err=%v, buf=%q", n, err, string(buf[:n]))
	}

	// Also verify GraphicsMode behaves identically without KD_GRAPHICS
	restore = GraphicsMode(int(w.Fd()))
	n, err = r.Read(buf[:])
	if err != nil || string(buf[:n]) != "\x1b[?25l" {
		t.Fatalf("expected \\x1b[?25l from GraphicsMode, got %q", string(buf[:n]))
	}
	restore()
	n, err = r.Read(buf[:])
	if err != nil || string(buf[:n]) != "\x1b[?25h" {
		t.Fatalf("expected \\x1b[?25h from GraphicsMode restore, got %q", string(buf[:n]))
	}
}

func TestKeyboardListenerPollTimeoutAndDrain(t *testing.T) {
	p1r, p1w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer p1r.Close()
	defer p1w.Close()

	l, err := NewKeyboardListener(int(p1r.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Timeout test
	buf, ok, eof := l.Poll(10 * time.Millisecond)
	if ok || eof || len(buf) > 0 {
		t.Fatalf("expected timeout: ok=%v eof=%v buf=%v", ok, eof, buf)
	}

	// Burst drain test
	_, _ = p1w.Write([]byte("abcdef"))
	buf, ok, eof = l.Poll(50 * time.Millisecond)
	if !ok || eof || string(buf) != "abcdef" {
		t.Fatalf("expected drained buffer 'abcdef', got %q (ok=%v, eof=%v)", string(buf), ok, eof)
	}

	// Close test
	_ = l.Close()
	buf, ok, eof = l.Poll(10 * time.Millisecond)
	if !eof {
		t.Fatalf("expected eof=true on closed listener, got ok=%v, eof=%v, buf=%v", ok, eof, buf)
	}

	// Verify that after Close(), no leaked goroutine steals subsequent bytes written to p1w
	if _, err := p1w.Write([]byte("post-close")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	var postBuf [16]byte
	n, err := p1r.Read(postBuf[:])
	if err != nil || string(postBuf[:n]) != "post-close" {
		t.Fatalf("expected 'post-close' to remain intact on primaryFd after Close(), got %q (err=%v)", string(postBuf[:n]), err)
	}
}

func TestDesktopMultiWindowZOrderMinimizeCloseAndCap(t *testing.T) {
	d := NewDesktop(1280, 800)
	defer func() {
		for _, w := range d.Windows {
			if w.Term != nil {
				_ = w.Term.Close()
			}
		}
	}()

	tel := d.findWindowByKind(WinTelemetry)
	term1 := d.findWindowByKind(WinTerminal)
	if tel == nil || term1 == nil {
		t.Skip("skipping multi-window test without PTY support")
	}

	// Initially terminal is on top and focused.
	if d.Windows[len(d.Windows)-1].ID != term1.ID || d.focusedID != term1.ID {
		t.Fatalf("expected terminal focused on top, got focused=%d top=%d", d.focusedID, d.Windows[len(d.Windows)-1].ID)
	}

	// Click inside Telemetry window brings it to front and focuses it.
	d.Update(input.State{X: tel.X + 10, Y: tel.Y + 40, Buttons: input.Left, W: 1280, H: 800})
	d.Update(input.State{X: tel.X + 10, Y: tel.Y + 40, Buttons: 0, W: 1280, H: 800})
	if d.Windows[len(d.Windows)-1].ID != tel.ID || d.focusedID != tel.ID {
		t.Fatalf("expected telemetry focused on top after click, got focused=%d top=%d", d.focusedID, d.Windows[len(d.Windows)-1].ID)
	}

	// Click minimize button [-] on Telemetry.
	mb := winMinButtonRect(tel)
	d.Update(input.State{X: mb.x + 2, Y: mb.y + 2, Buttons: input.Left, W: 1280, H: 800})
	d.Update(input.State{X: mb.x + 2, Y: mb.y + 2, Buttons: 0, W: 1280, H: 800})
	if !tel.Minimized || d.focusedID != term1.ID {
		t.Fatalf("expected telemetry minimized and term1 focused, min=%v focused=%d", tel.Minimized, d.focusedID)
	}

	// Click Telemetry task button in bottom dock to restore and focus it.
	var telDock rect
	for _, it := range d.dockItems(1280, 800) {
		if it.win != nil && it.win.ID == tel.ID {
			telDock = it.rect
			break
		}
	}
	d.Update(input.State{X: telDock.x + 4, Y: telDock.y + 4, Buttons: input.Left, W: 1280, H: 800})
	d.Update(input.State{X: telDock.x + 4, Y: telDock.y + 4, Buttons: 0, W: 1280, H: 800})
	if tel.Minimized || d.focusedID != tel.ID {
		t.Fatalf("expected telemetry restored and focused, min=%v focused=%d", tel.Minimized, d.focusedID)
	}

	// Spawn up to maxTerminals (4) and verify cap + cascading coordinates.
	t2 := d.SpawnTerminal()
	t3 := d.SpawnTerminal()
	t4 := d.SpawnTerminal()
	if t2 == nil || t3 == nil || t4 == nil {
		t.Fatal("failed to spawn terminals up to cap")
	}
	if t2.X == term1.X || t2.Y == term1.Y {
		t.Fatalf("expected cascading coordinates, got t1=(%d,%d) t2=(%d,%d)", term1.X, term1.Y, t2.X, t2.Y)
	}
	_ = d.SpawnTerminal() // should not exceed 4
	if d.terminalCount() != maxTerminals {
		t.Fatalf("expected terminal count capped at %d, got %d", maxTerminals, d.terminalCount())
	}

	// Close term1 via close button [x] and verify PTY is closed.
	d.FocusWindow(term1.ID)
	cb := winCloseButtonRect(term1)
	termObj := term1.Term
	d.Update(input.State{X: cb.x + 2, Y: cb.y + 2, Buttons: input.Left, W: 1280, H: 800})
	d.Update(input.State{X: cb.x + 2, Y: cb.y + 2, Buttons: 0, W: 1280, H: 800})
	if termObj.IsActive() {
		t.Fatal("expected closed terminal PTY to be inactive")
	}
	if d.terminalCount() != maxTerminals-1 {
		t.Fatalf("expected %d terminals after close, got %d", maxTerminals-1, d.terminalCount())
	}
}
