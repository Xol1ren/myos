package fb

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"myos/internal/sys"
)

const (
	feToolbarH = 28
	feRowH     = 20
	feMaxLines = 18
)

// FileEntry represents one directory entry in the graphical File Explorer.
type FileEntry struct {
	Name      string
	IsDir     bool
	Size      int64
	SizeHuman string
}

// FileExplorer holds state for the GUI file manager window.
type FileExplorer struct {
	Path         string
	Entries      []FileEntry
	Scroll       int
	Selected     int
	PreviewPath  string
	PreviewLines []string
	ErrMsg       string
}

// NewFileExplorer creates a FileExplorer rooted at startPath (defaulting to "/").
func NewFileExplorer(startPath string) *FileExplorer {
	if startPath == "" {
		startPath = "/"
	}
	fe := &FileExplorer{
		Path:     filepath.Clean(startPath),
		Selected: -1,
	}
	fe.Refresh()
	return fe
}

// Refresh reloads directory entries for fe.Path.
func (fe *FileExplorer) Refresh() {
	fe.ErrMsg = ""
	fe.Scroll = 0
	fe.Selected = -1
	fe.PreviewPath = ""
	fe.PreviewLines = nil

	dirEntries, err := os.ReadDir(fe.Path)
	if err != nil {
		fe.Entries = nil
		fe.ErrMsg = err.Error()
		return
	}

	entries := make([]FileEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		info, ierr := de.Info()
		var sz int64
		if ierr == nil {
			sz = info.Size()
		}
		isDir := de.IsDir()
		sizeHuman := "<DIR>"
		if !isDir {
			sizeHuman = sys.FormatBytes(uint64(max(sz, 0)))
		}
		entries = append(entries, FileEntry{
			Name:      de.Name(),
			IsDir:     isDir,
			Size:      sz,
			SizeHuman: sizeHuman,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	fe.Entries = entries
}

// NavigateUp moves to the parent directory.
func (fe *FileExplorer) NavigateUp() {
	parent := filepath.Dir(fe.Path)
	if parent != fe.Path {
		fe.Path = parent
		fe.Refresh()
	}
}

// OpenEntry navigates into a directory or opens a read-only preview for a regular file.
func (fe *FileExplorer) OpenEntry(idx int) {
	if idx < 0 || idx >= len(fe.Entries) {
		return
	}
	e := fe.Entries[idx]
	target := filepath.Join(fe.Path, e.Name)
	if e.IsDir {
		fe.Path = target
		fe.Refresh()
		return
	}
	fe.Selected = idx
	fe.loadPreview(target, e.Size)
}

func (fe *FileExplorer) loadPreview(path string, size int64) {
	fe.PreviewPath = path
	fe.PreviewLines = nil

	f, err := os.Open(path)
	if err != nil {
		fe.PreviewLines = []string{"Error: " + err.Error()}
		return
	}
	defer f.Close()

	var buf [2048]byte
	n, _ := f.Read(buf[:])
	data := buf[:n]

	if utf8.Valid(data) && !strings.ContainsRune(string(data), 0) {
		raw := strings.ReplaceAll(string(data), "\r\n", "\n")
		lines := strings.Split(raw, "\n")
		if len(lines) > feMaxLines {
			lines = lines[:feMaxLines]
		}
		for i := range lines {
			lines[i] = strings.ReplaceAll(lines[i], "\t", "    ")
		}
		fe.PreviewLines = lines
		return
	}

	// Binary fallback: file info + hex dump
	lines := []string{
		fmt.Sprintf("Binary file (%s)", sys.FormatBytes(uint64(max(size, 0)))),
		"",
	}
	limit := min(n, 64)
	for off := 0; off < limit; off += 8 {
		end := min(off+8, limit)
		var hexParts []string
		for _, b := range data[off:end] {
			hexParts = append(hexParts, fmt.Sprintf("%02x", b))
		}
		lines = append(lines, fmt.Sprintf("%04x: %s", off, strings.Join(hexParts, " ")))
	}
	fe.PreviewLines = lines
}

// ClosePreview closes the file preview modal/pane.
func (fe *FileExplorer) ClosePreview() {
	fe.PreviewPath = ""
	fe.PreviewLines = nil
}

func feUpBtnRect(w *Window) rect {
	return rect{w.X + 8, w.Y + titleBarH + 4, 62, 20}
}

func feScrollUpRect(w *Window) rect {
	return rect{w.X + w.W - 60, w.Y + titleBarH + 4, 24, 20}
}

func feScrollDnRect(w *Window) rect {
	return rect{w.X + w.W - 32, w.Y + titleBarH + 4, 24, 20}
}

func fePreviewCloseRect(w *Window) rect {
	return rect{w.X + 12, w.Y + w.H - 30, 76, 22}
}

func fePreviewEditRect(w *Window) rect {
	return rect{w.X + 96, w.Y + w.H - 30, 204, 22}
}

func (fe *FileExplorer) visibleRows(w *Window) int {
	avail := w.H - titleBarH - feToolbarH - 10
	if avail < feRowH {
		return 1
	}
	return avail / feRowH
}

// HandleClick processes a mouse click inside the File Explorer window.
// If the user clicks [Open in Terminal Editor], it returns the file path to edit.
func (fe *FileExplorer) HandleClick(px, py int, w *Window) string {
	if feUpBtnRect(w).contains(px, py) {
		if fe.PreviewPath != "" {
			fe.ClosePreview()
		} else {
			fe.NavigateUp()
		}
		return ""
	}

	if fe.PreviewPath != "" {
		if fePreviewCloseRect(w).contains(px, py) {
			fe.ClosePreview()
			return ""
		}
		if fePreviewEditRect(w).contains(px, py) {
			return fe.PreviewPath
		}
		return ""
	}

	if feScrollUpRect(w).contains(px, py) {
		if fe.Scroll > 0 {
			fe.Scroll--
		}
		return ""
	}
	if feScrollDnRect(w).contains(px, py) {
		vis := fe.visibleRows(w)
		if fe.Scroll+vis < len(fe.Entries) {
			fe.Scroll++
		}
		return ""
	}

	listY := w.Y + titleBarH + feToolbarH + 4
	if py >= listY && py < w.Y+w.H-6 {
		row := (py - listY) / feRowH
		idx := fe.Scroll + row
		if idx >= 0 && idx < len(fe.Entries) {
			fe.OpenEntry(idx)
		}
	}
	return ""
}

// Draw renders the File Explorer toolbar, entry list, or file preview pane.
func (fe *FileExplorer) Draw(c *Canvas, w *Window) {
	// Toolbar background
	tbY := w.Y + titleBarH
	c.DrawFillRect(w.X+1, tbY, w.W-2, feToolbarH, colTopBar)
	c.DrawFillRect(w.X+1, tbY+feToolbarH-1, w.W-2, 1, colTopBarLine)

	upR := feUpBtnRect(w)
	c.DrawFillRect(upR.x, upR.y, upR.w, upR.h, colButton)
	c.DrawRect(upR.x, upR.y, upR.w, upR.h, colAccent)
	c.DrawString(upR.x+6, upR.y+2, ".. Up", colText)

	// Path display
	pathX := upR.x + upR.w + 10
	maxPathChars := max((w.W-150)/GlyphW, 8)
	pathStr := fe.Path
	if len(pathStr) > maxPathChars {
		pathStr = "..." + pathStr[len(pathStr)-(maxPathChars-3):]
	}
	c.DrawString(pathX, upR.y+2, pathStr, colText)

	if fe.PreviewPath != "" {
		fe.drawPreview(c, w)
		return
	}

	// Scroll buttons
	su := feScrollUpRect(w)
	c.DrawFillRect(su.x, su.y, su.w, su.h, colButton)
	c.DrawRect(su.x, su.y, su.w, su.h, colTopBarLine)
	c.DrawString(su.x+8, su.y+2, "^", colText)

	sd := feScrollDnRect(w)
	c.DrawFillRect(sd.x, sd.y, sd.w, sd.h, colButton)
	c.DrawRect(sd.x, sd.y, sd.w, sd.h, colTopBarLine)
	c.DrawString(sd.x+8, sd.y+2, "v", colText)

	if fe.ErrMsg != "" {
		c.DrawString(w.X+12, tbY+feToolbarH+12, "Error: "+fe.ErrMsg, 0xE74C3C)
		return
	}

	vis := fe.visibleRows(w)
	listY := tbY + feToolbarH + 4
	maxNameChars := max((w.W-170)/GlyphW, 10)
	for i := 0; i < vis; i++ {
		idx := fe.Scroll + i
		if idx >= len(fe.Entries) {
			break
		}
		e := fe.Entries[idx]
		ry := listY + i*feRowH
		if idx == fe.Selected {
			c.DrawFillRect(w.X+4, ry, w.W-8, feRowH-2, colButtonDown)
		}
		tag := "[FILE]"
		tagCol := colDim
		if e.IsDir {
			tag = "[DIR]"
			tagCol = colAccent
		}
		c.DrawString(w.X+10, ry+2, tag, tagCol)

		name := e.Name
		if len(name) > maxNameChars {
			name = name[:maxNameChars-2] + ".."
		}
		c.DrawString(w.X+66, ry+2, name, colText)

		szX := w.X + w.W - len(e.SizeHuman)*GlyphW - 12
		c.DrawString(szX, ry+2, e.SizeHuman, colDim)
	}
}

func (fe *FileExplorer) drawPreview(c *Canvas, w *Window) {
	py := w.Y + titleBarH + feToolbarH + 8
	hdr := "Preview: " + filepath.Base(fe.PreviewPath)
	c.DrawString(w.X+12, py, hdr, colAccent)
	py += 22

	maxCols := max((w.W-24)/GlyphW, 10)
	maxY := w.Y + w.H - 38
	for _, line := range fe.PreviewLines {
		if py+GlyphH > maxY {
			break
		}
		if len(line) > maxCols {
			line = line[:maxCols]
		}
		c.DrawString(w.X+12, py, line, colText)
		py += GlyphH
	}

	cr := fePreviewCloseRect(w)
	c.DrawFillRect(cr.x, cr.y, cr.w, cr.h, colButton)
	c.DrawRect(cr.x, cr.y, cr.w, cr.h, colTopBarLine)
	c.DrawString(cr.x+16, cr.y+3, "Close", colText)

	er := fePreviewEditRect(w)
	c.DrawFillRect(er.x, er.y, er.w, er.h, colButton)
	c.DrawRect(er.x, er.y, er.w, er.h, colAccent)
	c.DrawString(er.x+10, er.y+3, "Open in Terminal Editor", colText)
}
