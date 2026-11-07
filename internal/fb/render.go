package fb

// Color is 0x00RRGGBB.
type Color = uint32

// Canvas is an in-memory 32-bit pixel surface. All primitives clip to its
// bounds and never allocate.
type Canvas struct {
	Pix  []uint32
	W, H int
}

// NewCanvas allocates a w*h canvas.
func NewCanvas(w, h int) Canvas { return Canvas{Pix: make([]uint32, w*h), W: w, H: h} }

// DrawPixel sets one pixel (ignored when out of bounds).
func (c *Canvas) DrawPixel(x, y int, col Color) {
	if x < 0 || y < 0 || x >= c.W || y >= c.H {
		return
	}
	c.Pix[y*c.W+x] = col
}

// DrawFillRect fills a w*h rectangle at (x,y).
func (c *Canvas) DrawFillRect(x, y, w, h int, col Color) {
	x0, y0, x1, y1 := max(x, 0), max(y, 0), min(x+w, c.W), min(y+h, c.H)
	if x0 >= x1 || y0 >= y1 {
		return
	}
	for yy := y0; yy < y1; yy++ {
		row := c.Pix[yy*c.W+x0 : yy*c.W+x1]
		for i := range row {
			row[i] = col
		}
	}
}

// DrawRect draws a 1px outline of a w*h rectangle at (x,y).
func (c *Canvas) DrawRect(x, y, w, h int, col Color) {
	if w <= 0 || h <= 0 {
		return
	}
	c.DrawFillRect(x, y, w, 1, col)
	c.DrawFillRect(x, y+h-1, w, 1, col)
	c.DrawFillRect(x, y, 1, h, col)
	c.DrawFillRect(x+w-1, y, 1, h, col)
}

// Glyph cell size of the embedded font.
const (
	GlyphW = 8
	GlyphH = 16
)

// DrawGlyph draws one ASCII character; unknown characters render as '?'.
func (c *Canvas) DrawGlyph(x, y int, ch byte, col Color) {
	if ch < 32 || ch > 126 {
		ch = '?'
	}
	g := font8x16[int(ch-32)*GlyphH : int(ch-32)*GlyphH+GlyphH]
	for row, bits := range g {
		if bits == 0 {
			continue
		}
		for bit := 0; bit < GlyphW; bit++ {
			if bits&(0x80>>bit) != 0 {
				c.DrawPixel(x+bit, y+row, col)
			}
		}
	}
}

// DrawString draws text with a transparent background and returns the x
// coordinate after the last glyph.
func (c *Canvas) DrawString(x, y int, s string, col Color) int {
	for i := 0; i < len(s); i++ {
		c.DrawGlyph(x, y, s[i], col)
		x += GlyphW
	}
	return x
}

// DrawBytes is DrawString for a byte slice (lets callers format text
// into a reused buffer without allocating).
func (c *Canvas) DrawBytes(x, y int, s []byte, col Color) int {
	for _, ch := range s {
		c.DrawGlyph(x, y, ch, col)
		x += GlyphW
	}
	return x
}
