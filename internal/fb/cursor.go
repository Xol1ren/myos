package fb

// Arrow sprite: 'X' black outline, 'O' white fill, ' ' transparent.
var arrowSprite = [...]string{
	"X           ",
	"XX          ",
	"XOX         ",
	"XOOX        ",
	"XOOOX       ",
	"XOOOOX      ",
	"XOOOOOX     ",
	"XOOOOOOX    ",
	"XOOOOOOOX   ",
	"XOOOOOOOOX  ",
	"XOOOOOXXXXX ",
	"XOOXOOX     ",
	"XOX XOOX    ",
	"XX  XOOX    ",
	"X    XOOX   ",
	"     XOOX   ",
	"      XX    ",
}

const (
	curW = 12
	curH = len(arrowSprite)
)

// Cursor draws the arrow over a canvas using save-under: Draw remembers the
// pixels it covers and Restore puts them back, so the canvas is never left
// with cursor pixels burned into it. Neither call allocates.
type Cursor struct {
	under  [curW * curH]uint32
	x, y   int
	clipX0 int
	clipY0 int
	clipX1 int
	clipY1 int
	active bool
}

// NewCursor returns a cursor with nothing drawn.
func NewCursor() *Cursor { return &Cursor{} }

// Draw paints the arrow with its hot spot at (x,y), saving the background.
// Call Restore before drawing it again.
// It clips the blit rectangle against canvas bounds [0, c.W) x [0, c.H)
// and guarantees zero out-of-range index panics when cursor touches display borders.
func (cu *Cursor) Draw(c *Canvas, x, y int) {
	if cu == nil {
		return
	}
	if cu.active {
		cu.Restore(c)
	}
	if c == nil || len(c.Pix) == 0 || c.W <= 0 || c.H <= 0 {
		return
	}

	cu.x, cu.y = x, y

	// Calculate clipped blit rectangle
	x0 := max(x, 0)
	y0 := max(y, 0)
	x1 := min(x+curW, c.W)
	y1 := min(y+curH, c.H)

	if x0 >= x1 || y0 >= y1 {
		// Entirely off-screen
		cu.clipX0, cu.clipY0, cu.clipX1, cu.clipY1 = 0, 0, 0, 0
		cu.active = false
		return
	}

	cu.clipX0, cu.clipY0, cu.clipX1, cu.clipY1 = x0, y0, x1, y1
	cu.active = true

	for py := y0; py < y1; py++ {
		r := py - y
		if r < 0 || r >= curH {
			continue
		}
		rowIdx := py * c.W
		for px := x0; px < x1; px++ {
			col := px - x
			if col < 0 || col >= curW {
				continue
			}
			idx := rowIdx + px
			if idx < 0 || idx >= len(c.Pix) {
				continue
			}
			cu.under[r*curW+col] = c.Pix[idx]
			switch arrowSprite[r][col] {
			case 'X':
				c.Pix[idx] = 0x000000
			case 'O':
				c.Pix[idx] = 0xFFFFFF
			}
		}
	}
}

// Restore puts back the pixels saved by the last Draw.
func (cu *Cursor) Restore(c *Canvas) {
	if cu == nil || !cu.active {
		return
	}
	cu.active = false
	if c == nil || len(c.Pix) == 0 || c.W <= 0 || c.H <= 0 {
		return
	}

	x0 := max(cu.clipX0, 0)
	y0 := max(cu.clipY0, 0)
	x1 := min(cu.clipX1, c.W)
	y1 := min(cu.clipY1, c.H)

	if x0 >= x1 || y0 >= y1 {
		return
	}

	for py := y0; py < y1; py++ {
		r := py - cu.y
		if r < 0 || r >= curH {
			continue
		}
		rowIdx := py * c.W
		for px := x0; px < x1; px++ {
			col := px - cu.x
			if col < 0 || col >= curW {
				continue
			}
			idx := rowIdx + px
			if idx >= 0 && idx < len(c.Pix) {
				if arrowSprite[r][col] != ' ' {
					c.Pix[idx] = cu.under[r*curW+col]
				}
			}
		}
	}
}

// DrawCursor is an alias for Draw with strict boundary clipping.
func (cu *Cursor) DrawCursor(c *Canvas, x, y int) {
	cu.Draw(c, x, y)
}

// RestoreCursor is an alias for Restore with strict boundary clipping.
func (cu *Cursor) RestoreCursor(c *Canvas) {
	cu.Restore(c)
}
