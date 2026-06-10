package v1

import (
	"net/http"
	"syscall"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// IndexHandler exposes indexer status and on-demand scan triggers.
type IndexHandler struct {
	svc        service.Services
	galleryDir string
}

// NewIndexHandler constructs an IndexHandler. galleryDir is the directory whose
// hosting partition is reported back to the UI for the storage indicator.
func NewIndexHandler(svc service.Services, galleryDir string) *IndexHandler {
	return &IndexHandler{svc: svc, galleryDir: galleryDir}
}

// Status returns current indexer statistics (counts + queue) plus the
// gallery directory and its hosting partition's total/available bytes so the UI
// can render a storage indicator that tracks the partition the gallery
// currently lives on (which may have been migrated to another disk/RAID).
//
// GET /v1/photos/status
func (h *IndexHandler) Status(c echo.Context) error {
	s := h.svc.Indexer().StatusCounts()
	s.MLReady = h.svc.Indexer().MLReady()
	s.GalleryDir = h.galleryDir
	if h.galleryDir != "" {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(h.galleryDir, &fs); err == nil {
			s.DiskTotal = int64(fs.Blocks) * int64(fs.Bsize)
			s.DiskAvail = int64(fs.Bavail) * int64(fs.Bsize)
		}
	}
	return c.JSON(http.StatusOK, s)
}

// Scan triggers a background scan of the default media directories.
// Returns immediately with 202 Accepted; the scan runs asynchronously.
//
// POST /v1/photos/scan
func (h *IndexHandler) Scan(c echo.Context) error {
	go func() {
		h.svc.Indexer().ScanAllRoots()
	}()
	return c.JSON(http.StatusAccepted, map[string]string{"status": "scanning"})
}
