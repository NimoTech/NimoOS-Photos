package v1

import (
	"errors"
	"net/http"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// Summary returns aggregated naming signals for one album (counts, date
// range, top places/persons, OCR and filename samples, cover candidates).
//
// GET /v1/photos/albums/:id/summary
func (h *AlbumsHandler) Summary(c echo.Context) error {
	sum, err := h.svc.Albums().Summary(c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, sum)
}
