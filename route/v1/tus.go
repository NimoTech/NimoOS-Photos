package v1

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/tus/tusd/v2/pkg/handler"
)

// freeBytesFn returns available bytes on /DATA. Injectable for tests.
type freeBytesFn func() (uint64, error)

// statfsDATA returns available bytes on /DATA via syscall.Statfs.
func statfsDATA() (uint64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs("/DATA", &s); err != nil {
		return 0, err
	}
	// Bavail = blocks available to unprivileged user
	return s.Bavail * uint64(s.Bsize), nil
}

// checkQuota returns an error if uploadLength (with 5% margin) would not fit.
// available is the free-bytes provider (statfsDATA in prod, mock in tests).
func checkQuota(uploadLength int64, available freeBytesFn) error {
	avail, err := available()
	if err != nil {
		return fmt.Errorf("storage check failed: %w", err)
	}
	needed := uint64(float64(uploadLength) * 1.05)
	if needed > avail {
		return fmt.Errorf("insufficient storage: need %d available %d", needed, avail)
	}
	return nil
}

// validateMetadataWithQuota checks metadata and quota with an injectable free-bytes provider.
func validateMetadataWithQuota(hook handler.HookEvent, quota freeBytesFn) (handler.HTTPResponse, error) {
	meta := hook.Upload.MetaData
	name := strings.TrimSpace(meta["filename"])
	if name == "" {
		return handler.HTTPResponse{}, fmt.Errorf("filename metadata required")
	}
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return handler.HTTPResponse{}, fmt.Errorf("filename contains illegal characters")
	}
	if hook.Upload.Size <= 0 {
		return handler.HTTPResponse{}, fmt.Errorf("empty file rejected")
	}
	if hook.Upload.Size > common.MaxUploadSize {
		return handler.HTTPResponse{}, fmt.Errorf("file exceeds %d byte limit", common.MaxUploadSize)
	}
	if err := checkQuota(hook.Upload.Size, quota); err != nil {
		return handler.HTTPResponse{StatusCode: 413}, err
	}
	return handler.HTTPResponse{}, nil
}

// validateMetadata is the production entry point used by tusd.
func validateMetadata(hook handler.HookEvent) (handler.HTTPResponse, error) {
	return validateMetadataWithQuota(hook, statfsDATA)
}

// ingestStagedFile moves a completed TUS upload from staging to the gallery
// and enqueues it for indexing. albumID may be empty.
// enqueue is injected (svc.Indexer().Enqueue in prod, fake in tests).
func ingestStagedFile(
	stagedPath string,
	filename string,
	albumID string,
	enqueue func(path string),
	galleryDir string,
) error {
	dest := filepath.Join(galleryDir, filename)
	// Try atomic rename first (same filesystem).
	if err := os.Rename(stagedPath, dest); err != nil {
		// Fallback: copy + delete (cross-fs case)
		if cerr := copyFile(stagedPath, dest); cerr != nil {
			return fmt.Errorf("rename and copy both failed: %w / %v", err, cerr)
		}
		os.Remove(stagedPath) //nolint:errcheck
	}
	// Remove .info sidecar
	os.Remove(stagedPath + ".info") //nolint:errcheck

	enqueue(dest)
	// albumID handling — current Indexer doesn't carry album metadata;
	// album assignment happens via a separate call after asset record exists.
	// Deferred to wiring task (Task 6).
	_ = albumID
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}
