package input

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestDecode(t *testing.T) {
	cases := []struct {
		name       string
		b0, b1, b2 byte
		dx, dy     int
		btn        uint8
		ok         bool
	}{
		{"idle", 0x08, 0, 0, 0, 0, 0, true},
		{"right+down-ish", 0x08, 5, 7, 5, 7, 0, true},
		{"left click", 0x09, 0, 0, 0, 0, Left, true},
		{"right click", 0x0a, 0, 0, 0, 0, Right, true},
		{"middle click", 0x0c, 0, 0, 0, 0, Middle, true},
		{"all buttons", 0x0f, 0, 0, 0, 0, Left | Right | Middle, true},
		{"negative x", 0x18, 0xff, 0, -1, 0, 0, true},
		{"negative y", 0x28, 0, 0xfe, 0, -2, 0, true},
		{"negative both", 0x38, 0x80, 0x80, -128, -128, 0, true},
		{"x overflow dropped", 0x48, 0x7f, 3, 0, 3, 0, true},
		{"y overflow dropped", 0x88, 3, 0x7f, 3, 0, 0, true},
		{"missing sync bit", 0x01, 1, 1, 0, 0, 0, false},
	}
	for _, c := range cases {
		dx, dy, btn, ok := Decode(c.b0, c.b1, c.b2)
		if dx != c.dx || dy != c.dy || btn != c.btn || ok != c.ok {
			t.Errorf("%s: got (%d,%d,%#x,%v) want (%d,%d,%#x,%v)", c.name, dx, dy, btn, ok, c.dx, c.dy, c.btn, c.ok)
		}
	}
}

func TestClampAndStartCentred(t *testing.T) {
	m := New("/nonexistent", 100, 50)
	if s := m.Snapshot(); s.X != 50 || s.Y != 25 || s.W != 100 || s.H != 50 {
		t.Fatalf("%+v", s)
	}
	m.apply(1000, 1000, 0)
	if s := m.Snapshot(); s.X != 99 || s.Y != 49 {
		t.Fatalf("upper clamp: %+v", s)
	}
	m.apply(-5000, -5000, 0)
	if s := m.Snapshot(); s.X != 0 || s.Y != 0 {
		t.Fatalf("lower clamp: %+v", s)
	}
	m.SetBounds(10, 10) // shrinking re-clamps; degenerate sizes are fixed up
	m.apply(100, 100, 0)
	m.SetBounds(5, 5)
	if s := m.Snapshot(); s.X != 4 || s.Y != 4 {
		t.Fatalf("SetBounds re-clamp: %+v", s)
	}
	m.SetBounds(0, -3)
	if s := m.Snapshot(); s.W != 1 || s.H != 1 || s.X != 0 || s.Y != 0 {
		t.Fatalf("degenerate bounds: %+v", s)
	}
}

func TestApplyDeltaAndButtonChange(t *testing.T) {
	m := New("/x", 200, 200)
	ch := m.Subscribe()
	m.apply(10, -4, Left)
	ev := <-ch
	if ev.X != 110 || ev.Y != 96 || ev.DX != 10 || ev.DY != -4 || ev.Buttons != Left || ev.Changed != Left {
		t.Fatalf("%+v", ev)
	}
	m.apply(0, 0, Left|Right)
	if ev := <-ch; ev.Changed != Right {
		t.Fatalf("changed=%#x", ev.Changed)
	}
	m.apply(0, 0, 0)
	if ev := <-ch; ev.Changed != Left|Right || ev.Buttons != 0 {
		t.Fatalf("%+v", ev)
	}
	m.Unsubscribe(ch)
	m.apply(1, 1, 0) // must not block or panic after unsubscribe
	if m.Snapshot().Packets != 4 {
		t.Fatal("packet count")
	}
}

func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	m := New("/x", 100, 100)
	m.Subscribe() // never drained, capacity 64
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			m.apply(1, 0, 0)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("apply blocked on a slow subscriber")
	}
}

// TestStreamFromFIFO drives the real read path (non-blocking open, epoll,
// resync on garbage) through a named pipe standing in for /dev/input/mice.
func TestStreamFromFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mice")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	m := New(path, 1000, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { m.Run(ctx); close(finished) }()

	w, err := os.OpenFile(path, os.O_WRONLY, 0) // blocks until the reader opens
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// leading garbage (no sync bit) must be skipped; then +10,+20 (PS/2 up
	// is positive, screen y flips) with left held, then a negative move.
	w.Write([]byte{0x00, 0x01, 0x02, 0x09, 10, 20, 0x38, 0xf6, 0xec})
	deadline := time.Now().Add(2 * time.Second)
	for m.Snapshot().Packets < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	s := m.Snapshot()
	if s.Packets != 2 || !s.Connected {
		t.Fatalf("%+v", s)
	}
	// start 500,500 ; pkt1: dx=10 dy=-20 ; pkt2: dx=-10 (0xf6) dy=+20 (0xec=-20 -> screen +20)
	if s.X != 500 || s.Y != 500 || s.Buttons != 0 {
		t.Fatalf("final %+v", s)
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

func TestRunRetriesMissingDeviceAndStops(t *testing.T) {
	m := New("/definitely/not/here", 10, 10)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { m.Run(ctx); close(finished) }()
	time.Sleep(50 * time.Millisecond)
	if m.Snapshot().Connected {
		t.Fatal("must not report connected")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Run ignored cancellation while retrying")
	}
}

func TestMouseBounds1280x800(t *testing.T) {
	m := New("/nonexistent", 1280, 800)
	if s := m.Snapshot(); s.W != 1280 || s.H != 800 || s.X != 640 || s.Y != 400 {
		t.Fatalf("initial 1280x800 state wrong: %+v", s)
	}

	m.apply(2000, 2000, 0)
	if s := m.Snapshot(); s.X != 1279 || s.Y != 799 {
		t.Fatalf("clamp at 1280x800 max: got (%d, %d), want (1279, 799)", s.X, s.Y)
	}

	// Dynamic update
	m.SetBounds(1920, 1080)
	if s := m.Snapshot(); s.W != 1920 || s.H != 1080 {
		t.Fatalf("dynamic SetBounds failed: %+v", s)
	}

	// Nil safety
	var nilMouse *Mouse
	nilMouse.SetBounds(1280, 800)
}
