package v1

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIngestStagedFile_AlbumID_SetPendingInvokedBeforeSubmit verifies that
// when albumID is non-empty, setPendingAlbum is called with the destination
// path (galleryDir+filename) and the albumID, and that it is called BEFORE
// submit (ordering verified via a shared event log).
func TestIngestStagedFile_AlbumID_SetPendingInvokedBeforeSubmit(t *testing.T) {
	stagingDir := t.TempDir()
	galleryDir := t.TempDir()

	stagedPath := filepath.Join(stagingDir, "upload1")
	if err := os.WriteFile(stagedPath, []byte("imgdata"), 0600); err != nil {
		t.Fatal(err)
	}

	const wantAlbumID = "album-abc-123"
	const wantFilename = "holiday.jpg"
	wantDest := filepath.Join(galleryDir, wantFilename)

	// Event log to verify ordering: "setPending" must appear before "submit".
	var events []string

	reserve := func(path, bid string, total int64) bool {
		return true
	}
	setPending := func(path, albumID string) {
		if path != wantDest {
			t.Errorf("setPendingAlbum: expected path %q, got %q", wantDest, path)
		}
		if albumID != wantAlbumID {
			t.Errorf("setPendingAlbum: expected albumID %q, got %q", wantAlbumID, albumID)
		}
		events = append(events, "setPending")
	}
	submit := func(path, bid string) {
		events = append(events, "submit")
	}

	err := ingestStagedFile(
		stagedPath, wantFilename, wantAlbumID,
		"batchX", 1,
		reserve, submit,
		setPending,
		galleryDir,
	)
	if err != nil {
		t.Fatalf("ingestStagedFile failed: %v", err)
	}

	// setPending must have been called exactly once.
	setPendingCalls := 0
	for _, e := range events {
		if e == "setPending" {
			setPendingCalls++
		}
	}
	if setPendingCalls != 1 {
		t.Errorf("setPendingAlbum should be called exactly once, got %d", setPendingCalls)
	}

	// Ordering: setPending before submit.
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events (setPending + submit), got %v", events)
	}
	setPendingIdx := -1
	submitIdx := -1
	for i, e := range events {
		if e == "setPending" {
			setPendingIdx = i
		}
		if e == "submit" {
			submitIdx = i
		}
	}
	if setPendingIdx == -1 {
		t.Error("setPendingAlbum was never called")
	}
	if submitIdx == -1 {
		t.Error("submit was never called")
	}
	if setPendingIdx >= submitIdx {
		t.Errorf("setPendingAlbum must be called BEFORE submit; events: %v", events)
	}
}

// TestIngestStagedFile_EmptyAlbumID_SetPendingNotInvoked verifies that when
// albumID is empty, setPendingAlbum is NOT invoked.
func TestIngestStagedFile_EmptyAlbumID_SetPendingNotInvoked(t *testing.T) {
	stagingDir := t.TempDir()
	galleryDir := t.TempDir()

	stagedPath := filepath.Join(stagingDir, "upload2")
	if err := os.WriteFile(stagedPath, []byte("imgdata"), 0600); err != nil {
		t.Fatal(err)
	}

	setPendingCalled := false
	setPending := func(path, albumID string) {
		setPendingCalled = true
	}
	reserve := func(path, bid string, total int64) bool { return true }
	submit := func(path, bid string) {}

	err := ingestStagedFile(
		stagedPath, "photo.jpg", "", // empty albumID
		"batchY", 1,
		reserve, submit,
		setPending,
		galleryDir,
	)
	if err != nil {
		t.Fatalf("ingestStagedFile failed: %v", err)
	}
	if setPendingCalled {
		t.Error("setPendingAlbum must NOT be called when albumID is empty")
	}
}
