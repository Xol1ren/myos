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
	active bool
}

// NewCursor returns a cursor with nothing drawn.
func NewCursor() *Cursor { return &Cursor{} }

// Draw paints the arrow with its hot spot at (x,y), saving the background.
// Call Restore before drawing it again.
func (cu *Cursor) Draw(c *Canvas, x, y int) {
	if cu.active {
		cu.Restore(c)
	}
	cu.x, cu.y, cu.active = x, y, true
	for r := 0; r < curH; r++ {
		for col := 0; col < curW; col++ {
			px, py := x+col, y+r
			if px < 0 || py < 0 || px >= c.W || py >= c.H {
				continue
			}
			idx := py*c.W + px
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
	if !cu.active {
		return
	}
	cu.active = false
	for r := 0; r < curH; r++ {
		for col := 0; col < curW; col++ {
			px, py := cu.x+col, cu.y+r
			if px < 0 || py < 0 || px >= c.W || py >= c.H {
				continue
			}
			if arrowSprite[r][col] != ' ' {
				c.Pix[py*c.W+px] = cu.under[r*curW+col]
			}
		}
	}
}
