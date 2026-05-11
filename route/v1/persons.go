package v1

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// PersonsHandler manages face-cluster persons and their associated assets.
type PersonsHandler struct{ svc service.Services }

// NewPersonsHandler constructs a PersonsHandler.
func NewPersonsHandler(svc service.Services) *PersonsHandler { return &PersonsHandler{svc} }

// List returns all known persons.
//
// GET /v1/photos/persons
func (h *PersonsHandler) List(c echo.Context) error {
	persons, err := h.svc.Search().ListPersons()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, persons)
}

// UpdateName sets a display name for a person.
//
// PUT /v1/photos/persons/:id
//
//	{ "name": "Alice" }
func (h *PersonsHandler) UpdateName(c echo.Context) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request body")
	}
	err := h.svc.Search().UpdatePersonName(c.Param("id"), req.Name)
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "updated"})
}

// Assets returns paginated assets associated with a person.
//
// GET /v1/photos/persons/:id/assets?limit=50&offset=0
func (h *PersonsHandler) Assets(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	assets, err := h.svc.Search().PersonAssets(c.Param("id"), limit, offset)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, assets)
}

// Merge merges one person cluster into another, then deletes the source.
//
// POST /v1/photos/persons/merge
//
//	{ "from_id": "<uuid>", "into_id": "<uuid>" }
func (h *PersonsHandler) Merge(c echo.Context) error {
	var req struct {
		FromID string `json:"from_id"`
		IntoID string `json:"into_id"`
	}
	if err := c.Bind(&req); err != nil || req.FromID == "" || req.IntoID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "from_id and into_id required")
	}
	if err := h.svc.Search().MergePersons(req.FromID, req.IntoID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "merged"})
}
