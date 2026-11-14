package fb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileExplorerNavigationAndPreview(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	txtFile := filepath.Join(sub, "hello.txt")
	if err := os.WriteFile(txtFile, []byte("line one\nline two\n"), 0644); err != nil {
		t.Fatal(err)
	}
	binFile := filepath.Join(root, "data.bin")
	if err := os.WriteFile(binFile, []byte{0x00, 0x01, 0xfe, 0xff}, 0644); err != nil {
		t.Fatal(err)
	}

	fe := NewFileExplorer(root)
	if len(fe.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(fe.Entries))
	}
	// Directories sorted first
	if !fe.Entries[0].IsDir || fe.Entries[0].Name != "sub" {
		t.Fatalf("expected first entry to be dir 'sub', got %+v", fe.Entries[0])
	}

	// Open directory
	fe.OpenEntry(0)
	if fe.Path != sub || len(fe.Entries) != 1 || fe.Entries[0].Name != "hello.txt" {
		t.Fatalf("unexpected state inside sub: path=%s entries=%+v", fe.Path, fe.Entries)
	}

	// Open text file preview
	fe.OpenEntry(0)
	if fe.PreviewPath != txtFile || len(fe.PreviewLines) < 2 || fe.PreviewLines[0] != "line one" {
		t.Fatalf("unexpected text preview: path=%s lines=%v", fe.PreviewPath, fe.PreviewLines)
	}

	// Simulate clicking [Open in Terminal Editor]
	w := &Window{X: 20, Y: 40, W: 500, H: 360, Title: "File Explorer", Files: fe}
	c := NewCanvas(640, 480)
	fe.Draw(&c, w)

	er := fePreviewEditRect(w)
	gotEdit := fe.HandleClick(er.x+5, er.y+5, w)
	if gotEdit != txtFile {
		t.Fatalf("expected edit path %q, got %q", txtFile, gotEdit)
	}

	// Close preview and navigate up
	cr := fePreviewCloseRect(w)
	fe.HandleClick(cr.x+5, cr.y+5, w)
	if fe.PreviewPath != "" {
		t.Fatal("expected preview closed")
	}

	up := feUpBtnRect(w)
	fe.HandleClick(up.x+5, up.y+5, w)
	if fe.Path != root {
		t.Fatalf("expected path %q after Up, got %q", root, fe.Path)
	}

	// Open binary file preview
	fe.OpenEntry(1)
	if fe.PreviewPath != binFile || len(fe.PreviewLines) == 0 || !strings.HasPrefix(fe.PreviewLines[0], "Binary file") {
		t.Fatalf("unexpected binary preview: %v", fe.PreviewLines)
	}
}
