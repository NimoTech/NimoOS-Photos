package v1

import (
	"net/http"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// IndexHandler exposes indexer status and on-demand scan triggers.
type IndexHandler struct{ svc service.Services }

// NewIndexHandler constructs an IndexHandler.
func NewIndexHandler(svc service.Services) *IndexHandler { return &IndexHandler{svc} }

// Status returns current indexer statistics (pending / indexed / error counts
// and the current queue length).
//
// GET /v1/photos/status
func (h *IndexHandler) Status(c echo.Context) error {
	return c.JSON(http.StatusOK, h.svc.Indexer().StatusCounts())
}

// Scan triggers a background scan of the default media directories.
// Returns immediately with 202 Accepted; the scan runs asynchronously.
//
// POST /v1/photos/scan
func (h *IndexHandler) Scan(c echo.Context) error {
	go func() {
		for _, dir := range []string{"/DATA/Gallery", "/DATA/Documents", "/DATA/Downloads"} {
			h.svc.Indexer().ScanDirectory(dir) //nolint:errcheck
		}
	}()
	return c.JSON(http.StatusAccepted, map[string]string{"status": "scanning"})
}
