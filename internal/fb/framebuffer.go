// Package fb is a zero-dependency framebuffer GUI: it mmaps /dev/fb0,
// draws into an offscreen canvas and blits whole frames to video memory.
package fb

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"myos/internal/sys"
)

const (
	fbioGetVScreenInfo = 0x4600
	fbioGetFScreenInfo = 0x4602
	kdSetMode          = 0x4B3A
	kdText             = 0x00
	kdGraphics         = 0x01
)

type bitfield struct{ offset, length, msbRight uint32 }

// fbVarScreenInfo mirrors struct fb_var_screeninfo (160 bytes).
type fbVarScreenInfo struct {
	xres, yres, xresVirtual, yresVirtual uint32
	xoffset, yoffset                     uint32
	bitsPerPixel, grayscale              uint32
	red, green, blue, transp             bitfield
	nonstd, activate, height, width      uint32
	accelFlags                           uint32
	pixclock                             uint32
	leftMargin, rightMargin              uint32
	upperMargin, lowerMargin             uint32
	hsyncLen, vsyncLen                   uint32
	sync, vmode, rotate, colorspace      uint32
	reserved                             [4]uint32
}

// fbFixScreenInfo mirrors struct fb_fix_screeninfo on 64-bit Linux (72 bytes).
type fbFixScreenInfo struct {
	id                            [16]byte
	smemStart                     uint64
	smemLen                       uint32
	typ, typeAux, visual          uint32
	xpanstep, ypanstep, ywrapstep uint16
	lineLength                    uint32
	mmioStart                     uint64
	mmioLen                       uint32
	accel                         uint32
	capabilities                  uint16
	reserved                      [2]uint16
}

// Framebuffer is an opened and mapped /dev/fb0 plus its offscreen canvas.
type Framebuffer struct {
	Canvas // offscreen back buffer, 0x00RRGGBB pixels

	file   *os.File
	mem    []byte // mmap of video RAM (nil for test framebuffers)
	base   int    // byte offset of the visible area inside mem
	stride int    // bytes per hardware scanline
	bpp    int

	rOff, gOff, bOff uint
	rLen, gLen, bLen uint
	fast32           bool // hardware is 32bpp XRGB: rows can be copied directly
	hw32             []uint32
	stride32         int
}

// Open maps the framebuffer device at path (usually /dev/fb0).
func Open(path string) (*Framebuffer, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var vi fbVarScreenInfo
	var fi fbFixScreenInfo
	fd := int(f.Fd())
	if err := sys.Ioctl(fd, fbioGetVScreenInfo, unsafe.Pointer(&vi)); err != nil {
		f.Close()
		return nil, fmt.Errorf("FBIOGET_VSCREENINFO: %w", err)
	}
	if err := sys.Ioctl(fd, fbioGetFScreenInfo, unsafe.Pointer(&fi)); err != nil {
		f.Close()
		return nil, fmt.Errorf("FBIOGET_FSCREENINFO: %w", err)
	}
	mem, err := syscall.Mmap(fd, 0, int(fi.smemLen), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmap %s: %w", path, err)
	}
	b := &Framebuffer{file: f, mem: mem}
	if err := b.init(vi, fi); err != nil {
		_ = syscall.Munmap(mem)
		f.Close()
		return nil, err
	}
	return b, nil
}

// init derives geometry and pixel format and allocates the back buffer once.
func (b *Framebuffer) init(vi fbVarScreenInfo, fi fbFixScreenInfo) error {
	bpp := int(vi.bitsPerPixel)
	if bpp != 16 && bpp != 24 && bpp != 32 {
		return fmt.Errorf("unsupported framebuffer depth: %d bpp", bpp)
	}
	w, h := int(vi.xres), int(vi.yres)
	if w == 0 || h == 0 {
		return fmt.Errorf("framebuffer reports empty resolution")
	}
	b.bpp, b.stride = bpp, int(fi.lineLength)
	b.base = int(vi.yoffset)*b.stride + int(vi.xoffset)*bpp/8
	if need := b.base + (h-1)*b.stride + w*bpp/8; b.stride < w*bpp/8 || need > len(b.mem) {
		return fmt.Errorf("framebuffer geometry exceeds mapped memory (%d > %d)", need, len(b.mem))
	}
	b.rOff, b.rLen = uint(vi.red.offset), uint(vi.red.length)
	b.gOff, b.gLen = uint(vi.green.offset), uint(vi.green.length)
	b.bOff, b.bLen = uint(vi.blue.offset), uint(vi.blue.length)
	b.fast32 = bpp == 32 && b.rOff == 16 && b.gOff == 8 && b.bOff == 0 &&
		b.rLen == 8 && b.gLen == 8 && b.bLen == 8 && b.stride%4 == 0 && b.base%4 == 0 && len(b.mem) >= 4
	if b.fast32 {
		b.hw32 = unsafe.Slice((*uint32)(unsafe.Pointer(&b.mem[0])), len(b.mem)/4)
		b.stride32 = b.stride / 4
	}
	b.Canvas = NewCanvas(w, h)
	return nil
}

// Width and Height of the visible resolution.
func (b *Framebuffer) Width() int  { return b.Canvas.W }
func (b *Framebuffer) Height() int { return b.Canvas.H }

// Surface returns the pointer to the underlying back-buffer canvas.
func (b *Framebuffer) Surface() *Canvas { return &b.Canvas }

// Backend returns the human-readable backend descriptor.
func (b *Framebuffer) Backend() string { return "Framebuffer (/dev/fb0)" }

// BPP is the hardware bits per pixel.
func (b *Framebuffer) BPP() int { return b.bpp }

// Blit copies the offscreen canvas to video memory in one pass. It performs
// no allocations.
func (b *Framebuffer) Blit() {
	w, h := b.Canvas.W, b.Canvas.H
	src := b.Canvas.Pix
	if b.fast32 {
		o := b.base / 4
		for y := 0; y < h; y++ {
			copy(b.hw32[o+y*b.stride32:o+y*b.stride32+w], src[y*w:(y+1)*w])
		}
		return
	}
	for y := 0; y < h; y++ {
		row := b.mem[b.base+y*b.stride:]
		s := src[y*w : (y+1)*w]
		switch b.bpp {
		case 32:
			for x, p := range s {
				v := b.pack(p)
				i := x * 4
				row[i], row[i+1], row[i+2], row[i+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
			}
		case 24:
			for x, p := range s {
				v := b.pack(p)
				i := x * 3
				row[i], row[i+1], row[i+2] = byte(v), byte(v>>8), byte(v>>16)
			}
		case 16:
			for x, p := range s {
				v := b.pack(p)
				i := x * 2
				row[i], row[i+1] = byte(v), byte(v>>8)
			}
		}
	}
}

// pack converts 0x00RRGGBB to the hardware pixel layout.
func (b *Framebuffer) pack(p uint32) uint32 {
	r, g, bl := (p>>16)&0xff, (p>>8)&0xff, p&0xff
	return r>>(8-b.rLen)<<b.rOff | g>>(8-b.gLen)<<b.gOff | bl>>(8-b.bLen)<<b.bOff
}

// Close unmaps video memory and closes the device. It is safe to call twice.
func (b *Framebuffer) Close() error {
	var err error
	if b.mem != nil {
		b.hw32 = nil
		err = syscall.Munmap(b.mem)
		b.mem = nil
	}
	if b.file != nil {
		if e := b.file.Close(); err == nil {
			err = e
		}
		b.file = nil
	}
	return err
}

// GraphicsMode switches the virtual terminal on fd to KD_GRAPHICS so the
// kernel console stops drawing over the GUI. The returned function restores
// KD_TEXT. If fd is not a VT (e.g. a serial console) it is a no-op.
func GraphicsMode(fd int) (restore func()) {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), kdSetMode, kdGraphics); e != 0 {
		return func() {}
	}
	return func() { syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), kdSetMode, kdText) }
}
