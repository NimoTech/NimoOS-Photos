package v1

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tus/tusd/v2/pkg/handler"
)

func TestValidateMetadata(t *testing.T) {
	cases := []struct {
		name    string
		meta    map[string]string
		wantErr bool
	}{
		{"empty filename", map[string]string{"filename": ""}, true},
		{"missing filename", map[string]string{}, true},
		{"path traversal", map[string]string{"filename": "../etc/passwd"}, true},
		{"slash in name", map[string]string{"filename": "a/b.jpg"}, true},
		{"normal filename", map[string]string{"filename": "IMG_3421.HEIC"}, false},
		{"unicode filename", map[string]string{"filename": "照片_2026.jpg"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := handler.HookEvent{
				Upload: handler.FileInfo{MetaData: tc.meta, Size: 1000},
			}
			_, _, err := validateMetadataWithQuota(info, func() (uint64, error) { return 1 << 40, nil })
			if (err != nil) != tc.wantErr {
				t.Fatalf("wantErr=%v got err=%v", tc.wantErr, err)
			}
		})
	}
}

func TestValidateMetadata_SizeLimit(t *testing.T) {
	info := handler.HookEvent{
		Upload: handler.FileInfo{
			MetaData: map[string]string{"filename": "x.jpg"},
			Size:     21 * 1024 * 1024 * 1024, // 21 GB
		},
	}
	_, _, err := validateMetadataWithQuota(info, func() (uint64, error) { return 1 << 40, nil })
	if err == nil {
		t.Fatal("expected error for oversized upload, got nil")
	}
}

func TestValidateMetadata_ZeroSize(t *testing.T) {
	info := handler.HookEvent{
		Upload: handler.FileInfo{
			MetaData: map[string]string{"filename": "x.jpg"},
			Size:     0,
		},
	}
	_, _, err := validateMetadata(info)
	if err == nil {
		t.Fatal("expected error for zero-size upload, got nil")
	}
}

func TestCheckQuota_Sufficient(t *testing.T) {
	err := checkQuota(1000, func() (uint64, error) { return 1_000_000, nil })
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestCheckQuota_Insufficient(t *testing.T) {
	// uploadLength * 1.05 > available
	err := checkQuota(1000, func() (uint64, error) { return 1000, nil })
	if err == nil {
		t.Fatal("expected quota error, got nil")
	}
}

func TestCheckQuota_StatFails(t *testing.T) {
	err := checkQuota(1000, func() (uint64, error) {
		return 0, fmt.Errorf("statfs failed")
	})
	if err == nil {
		t.Fatal("expected error when statfs fails")
	}
}

func TestIngestStagedFile_Success(t *testing.T) {
	stagingDir := t.TempDir()
	galleryDir := t.TempDir()

	// Create a fake staged file
	stagedID := "abc123"
	stagedPath := filepath.Join(stagingDir, stagedID)
	stagedInfo := stagedPath + ".info"
	if err := os.WriteFile(stagedPath, []byte("imagedata"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagedInfo, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	const wantBatchID = "batch-uuid-001"
	const wantBatchTotal int64 = 5

	var reservedPath, reservedBatchID string
	var reservedBatchTotal int64
	reserve := func(path, bid string, total int64) bool {
		reservedPath = path
		reservedBatchID = bid
		reservedBatchTotal = total
		return true
	}

	var submittedPath, submittedBatchID string
	submit := func(path, bid string) {
		submittedPath = path
		submittedBatchID = bid
	}

	noopSetPending := func(path, albumID string) {}
	err := ingestStagedFile(
		stagedPath, "IMG_001.jpg", "",
		wantBatchID, wantBatchTotal,
		reserve, submit,
		noopSetPending,
		galleryDir,
	)
	if err != nil {
		t.Fatalf("ingest failed: %v", err)
	}

	// Original moved out of staging
	if _, e := os.Stat(stagedPath); !os.IsNotExist(e) {
		t.Error("staged file should be removed")
	}
	if _, e := os.Stat(stagedInfo); !os.IsNotExist(e) {
		t.Error(".info file should be removed")
	}

	// Should be in gallery
	expectedDest := filepath.Join(galleryDir, "IMG_001.jpg")
	if _, e := os.Stat(expectedDest); e != nil {
		t.Errorf("expected file at %s: %v", expectedDest, e)
	}

	// reserve must have been called with correct args before rename
	if reservedPath != expectedDest {
		t.Errorf("reserve: expected path %s, got %s", expectedDest, reservedPath)
	}
	if reservedBatchID != wantBatchID {
		t.Errorf("reserve: expected batchID %q, got %q", wantBatchID, reservedBatchID)
	}
	if reservedBatchTotal != wantBatchTotal {
		t.Errorf("reserve: expected batchTotal %d, got %d", wantBatchTotal, reservedBatchTotal)
	}

	// submit must have been called with correct path + batchID
	if submittedPath != expectedDest {
		t.Errorf("submit: expected path %s, got %s", expectedDest, submittedPath)
	}
	if submittedBatchID != wantBatchID {
		t.Errorf("submit: expected batchID %q, got %q", wantBatchID, submittedBatchID)
	}
}

func TestIngestStagedFile_ReserveFailure(t *testing.T) {
	stagingDir := t.TempDir()
	galleryDir := t.TempDir()

	stagedPath := filepath.Join(stagingDir, "xyz")
	if err := os.WriteFile(stagedPath, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}

	// reserve returns false — simulates path already occupied
	reserve := func(path, bid string, total int64) bool { return false }
	submit := func(path, bid string) { t.Error("submit must not be called when reserve fails") }

	err := ingestStagedFile(stagedPath, "photo.jpg", "", "bid", 1, reserve, submit, func(_, _ string) {}, galleryDir)
	if err == nil {
		t.Fatal("expected error when reserve returns false, got nil")
	}
}
