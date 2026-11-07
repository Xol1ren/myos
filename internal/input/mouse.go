// Package input reads the kernel's /dev/input/mice multiplexed PS/2 mouse
// stream and tracks a clamped cursor position and button state.
package input

import (
	"bufio"
	"context"
	"os"
	"sync"
	"syscall"
	"time"
)

// Button bit flags (match PS/2 byte 1 bits 0..2).
const (
	Left   = 1 << 0
	Right  = 1 << 1
	Middle = 1 << 2
)

// Event is one decoded mouse packet.
type Event struct {
	DX, DY  int // movement; DY is positive downwards (screen convention)
	X, Y    int // clamped absolute position after applying this packet
	Buttons uint8
	Changed uint8 // buttons whose state changed in this packet
}

// State is a snapshot of the tracker.
type State struct {
	X, Y, W, H int
	Buttons    uint8
	Packets    uint64
	Connected  bool
}

// Mouse tracks pointer state from a /dev/input/mice style device.
type Mouse struct {
	path string

	mu      sync.Mutex
	w, h    int
	x, y    int
	btn     uint8
	packets uint64
	connect bool
	subs    map[chan Event]struct{}
}

// New creates a tracker clamped to [0,width)x[0,height), starting centred.
func New(path string, width, height int) *Mouse {
	m := &Mouse{path: path, subs: map[chan Event]struct{}{}}
	m.SetBounds(width, height)
	m.x, m.y = m.w/2, m.h/2
	return m
}

// SetBounds changes the clamping rectangle.
func (m *Mouse) SetBounds(width, height int) {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	m.mu.Lock()
	m.w, m.h = width, height
	m.x, m.y = clamp(m.x, 0, width-1), clamp(m.y, 0, height-1)
	m.mu.Unlock()
}

// Snapshot returns the current state.
func (m *Mouse) Snapshot() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return State{m.x, m.y, m.w, m.h, m.btn, m.packets, m.connect}
}

// Subscribe returns a channel of events. Slow consumers drop events.
func (m *Mouse) Subscribe() chan Event {
	ch := make(chan Event, 64)
	m.mu.Lock()
	m.subs[ch] = struct{}{}
	m.mu.Unlock()
	return ch
}

// Unsubscribe removes a channel returned by Subscribe.
func (m *Mouse) Unsubscribe(ch chan Event) {
	m.mu.Lock()
	delete(m.subs, ch)
	m.mu.Unlock()
}

// Decode converts a 3-byte PS/2 packet. Byte 1: bit0 L, bit1 R, bit2 M,
// bit3 always 1, bit4 X sign, bit5 Y sign, bit6 X overflow, bit7 Y overflow.
// dy is returned in PS/2 orientation (positive = up). ok is false if the
// sync bit is missing.
func Decode(b0, b1, b2 byte) (dx, dy int, buttons uint8, ok bool) {
	if b0&0x08 == 0 {
		return 0, 0, 0, false
	}
	buttons = b0 & 0x07
	dx, dy = int(b1), int(b2)
	if b0&0x10 != 0 {
		dx -= 256
	}
	if b0&0x20 != 0 {
		dy -= 256
	}
	if b0&0x40 != 0 { // overflow: delta is meaningless
		dx = 0
	}
	if b0&0x80 != 0 {
		dy = 0
	}
	return dx, dy, buttons, true
}

// Run reads packets until ctx is cancelled. If the device is missing or
// disappears it retries every second (USB devices may enumerate late).
func (m *Mouse) Run(ctx context.Context) {
	for ctx.Err() == nil {
		m.stream(ctx)
		m.setConnected(false)
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (m *Mouse) setConnected(c bool) {
	m.mu.Lock()
	m.connect = c
	m.mu.Unlock()
}

func (m *Mouse) stream(ctx context.Context) {
	fd, err := syscall.Open(m.path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	// A non-blocking fd is registered with Go's epoll poller, so Read parks
	// the goroutine instead of burning a thread, and Close interrupts it.
	f := os.NewFile(uintptr(fd), m.path)
	// The watcher goroutine ends when either the context is cancelled or this
	// stream returns, so reconnect loops never accumulate goroutines.
	done := make(chan struct{})
	defer close(done)
	defer f.Close()
	go func() {
		select {
		case <-ctx.Done():
			f.Close()
		case <-done:
		}
	}()
	m.setConnected(true)

	r := bufio.NewReaderSize(f, 64)
	for {
		b0, err := r.ReadByte()
		if err != nil {
			return
		}
		if b0&0x08 == 0 { // out of sync: skip until a plausible first byte
			continue
		}
		var rest [2]byte
		for i := range rest {
			if rest[i], err = r.ReadByte(); err != nil {
				return
			}
		}
		if dx, dy, btn, ok := Decode(b0, rest[0], rest[1]); ok {
			m.apply(dx, -dy, btn)
		}
	}
}

func (m *Mouse) apply(dx, dy int, btn uint8) {
	m.mu.Lock()
	m.x = clamp(m.x+dx, 0, m.w-1)
	m.y = clamp(m.y+dy, 0, m.h-1)
	ev := Event{DX: dx, DY: dy, X: m.x, Y: m.y, Buttons: btn, Changed: btn ^ m.btn}
	m.btn = btn
	m.packets++
	for ch := range m.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	m.mu.Unlock()
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
