package v1

import (
	"net/http"
	"strconv"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// SearchHandler handles semantic search and person-based asset search.
type SearchHandler struct{ svc service.Services }

// NewSearchHandler constructs a SearchHandler.
func NewSearchHandler(svc service.Services) *SearchHandler { return &SearchHandler{svc} }

// Smart performs CLIP-based semantic search.
//
// POST /v1/photos/search/smart
//
//	{
//	  "query":   "sunset at the beach",
//	  "limit":   20,
//	  "filters": { "year": 2024, "month": 7 }
//	}
func (h *SearchHandler) Smart(c echo.Context) error {
	var req struct {
		Query   string                `json:"query"`
		Filters service.SearchFilters `json:"filters"`
		Limit   int                   `json:"limit"`
	}
	if err := c.Bind(&req); err != nil || req.Query == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "query is required")
	}
	if req.Limit <= 0 || req.Limit > 100 {
		req.Limit = 20
	}
	// The search box always matches recognized text too; Smart View semantic
	// conditions call SmartSearch directly and stay pure-CLIP.
	req.Filters.IncludeOCR = true
	results, err := h.svc.Search().SmartSearch(req.Query, req.Limit, req.Filters)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, results)
}

// ByPerson returns paginated assets containing a given person's face.
//
// GET /v1/photos/search/faces/:person_id?limit=50&offset=0
func (h *SearchHandler) ByPerson(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	results, err := h.svc.Search().PersonAssets(c.Param("person_id"), limit, offset)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, results)
}
