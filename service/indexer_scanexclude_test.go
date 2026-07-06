package service

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestWalkSupportedSkipsDATASystemDirs(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "DATA")
	oldExcl := scanExcludeDirs
	scanExcludeDirs = map[string]bool{
		filepath.Join(data, "AppData"):    true,
		filepath.Join(data, "lost+found"): true,
	}
	defer func() { scanExcludeDirs = oldExcl }()

	mk := func(rel string) {
		p := filepath.Join(data, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("Documents/a.jpg")
	mk("AppData/app/b.jpg")
	mk("lost+found/c.png")

	var got []string
	if err := walkSupported(context.Background(), data, func(p string) {
		rel, _ := filepath.Rel(data, p)
		got = append(got, rel)
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if len(got) != 1 || got[0] != "Documents/a.jpg" {
		t.Fatalf("got=%v want=[Documents/a.jpg]", got)
	}
}
