package shell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTextEditorEditAndSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "hello.txt")
	ed, err := newTextEditor(path)
	if err != nil {
		t.Fatalf("newTextEditor: %v", err)
	}
	for _, r := range "hello" {
		ed.insertRune(r)
	}
	ed.insertNewline()
	for _, r := range "world!" {
		ed.insertRune(r)
	}
	ed.backspace() // remove '!'
	ed.moveHome()
	ed.deleteChar() // remove 'w'
	ed.insertRune('W')

	if !ed.dirty {
		t.Fatal("expected editor to be dirty")
	}
	n, err := ed.save()
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if n == 0 || ed.dirty {
		t.Fatalf("unexpected save state: n=%d dirty=%v", n, ed.dirty)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello\nWorld\n" {
		t.Fatalf("got %q, want %q", string(got), "hello\nWorld\n")
	}
}

func TestTextEditorNavigationAndScroll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nav.txt")
	if err := os.WriteFile(path, []byte("line1\nline2\nline3\nline4\nline5\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ed, err := newTextEditor(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ed.lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(ed.lines))
	}

	ed.moveDown()
	ed.moveEnd()
	if ed.row != 1 || ed.col != 5 {
		t.Fatalf("unexpected pos: %d,%d", ed.row, ed.col)
	}
	ed.pageDown(3)
	if ed.row != 4 {
		t.Fatalf("expected row 4 after pageDown, got %d", ed.row)
	}
	ed.pageUp(3)
	if ed.row != 1 {
		t.Fatalf("expected row 1 after pageUp, got %d", ed.row)
	}
	out := ed.render(4, 80)
	if !strings.Contains(out, "Ctrl+S: Save") || !strings.Contains(out, "nav.txt") {
		t.Fatalf("status bar missing expected text: %q", out)
	}
}
