package v1

import (
	"database/sql"
	"net/http"
	"os"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// AboutHandler exposes version / device / library metadata for the settings page.
type AboutHandler struct{ svc service.Services }

// NewAboutHandler constructs an AboutHandler.
func NewAboutHandler(svc service.Services) *AboutHandler { return &AboutHandler{svc} }

// GET /v1/photos/about
func (h *AboutHandler) Get(c echo.Context) error {
	db := h.svc.DB()

	var librarySince, lastBuilt sql.NullString
	_ = db.QueryRow(`SELECT MIN(indexed_at) FROM assets WHERE status='indexed'`).Scan(&librarySince)
	_ = db.QueryRow(`SELECT value FROM photos_meta WHERE key='index_last_rebuilt'`).Scan(&lastBuilt)

	var coverage int
	_ = db.QueryRow(`SELECT COUNT(*) FROM asset_clip_idx i
		JOIN assets a ON a.id = i.asset_id
		WHERE a.status='indexed' AND a.deleted_at IS NULL`).Scan(&coverage)

	host, _ := os.Hostname()

	return c.JSON(http.StatusOK, map[string]interface{}{
		"version":        common.PhotosVersion,
		"deviceName":     host,
		"librarySince":   nullableStr(librarySince),
		"indexCoverage":  coverage,
		"indexLastBuilt": nullableStr(lastBuilt),
	})
}

// nullableStr maps an empty sql.NullString to JSON null.
func nullableStr(s sql.NullString) interface{} {
	if s.Valid && s.String != "" {
		return s.String
	}
	return nil
}
