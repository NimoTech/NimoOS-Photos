package v1

import (
	"fmt"
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
