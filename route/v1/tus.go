package v1

import (
	"fmt"
	"strings"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/tus/tusd/v2/pkg/handler"
)

// validateMetadata checks PreUploadCreate metadata for safety and limits.
// Returns the (possibly modified) FileInfoChanges and an error that, if non-nil,
// causes tusd to reject the upload before any bytes are accepted.
func validateMetadata(hook handler.HookEvent) (handler.HTTPResponse, error) {
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
	return handler.HTTPResponse{}, nil
}
