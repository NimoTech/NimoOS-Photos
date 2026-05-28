package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneStaging_RemovesOldFiles(t *testing.T) {
	dir := t.TempDir()

	old := filepath.Join(dir, "old.bin")
	oldInfo := old + ".info"
	if err := os.WriteFile(old, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldInfo, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	weekAgo := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(old, weekAgo, weekAgo)
	os.Chtimes(oldInfo, weekAgo, weekAgo)

	fresh := filepath.Join(dir, "fresh.bin")
	freshInfo := fresh + ".info"
	if err := os.WriteFile(fresh, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(freshInfo, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	removed, err := PruneStaging(dir, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if removed != 2 {
		t.Errorf("expected 2 removed, got %d", removed)
	}
	if _, e := os.Stat(old); !os.IsNotExist(e) {
		t.Error("old file should be gone")
	}
	if _, e := os.Stat(fresh); e != nil {
		t.Error("fresh file should remain")
	}
}

func TestPruneStaging_MissingDir(t *testing.T) {
	removed, err := PruneStaging("/nonexistent/path", time.Hour)
	if err != nil {
		t.Errorf("missing dir should not error, got %v", err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removed, got %d", removed)
	}
}
