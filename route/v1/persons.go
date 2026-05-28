package v1

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// PersonsHandler 提供 People 相关 HTTP 端点。
type PersonsHandler struct {
	svc          service.Services
	faceThumbDir string
	thumbDir     string
	ctx          context.Context
}

func NewPersonsHandler(svc service.Services, faceThumbDir, thumbDir string, ctx context.Context) *PersonsHandler {
	return &PersonsHandler{svc: svc, faceThumbDir: faceThumbDir, thumbDir: thumbDir, ctx: ctx}
}

// GET /v1/photos/persons
func (h *PersonsHandler) List(c echo.Context) error {
	persons, err := h.svc.Persons().ListPersons()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	indexedUpTo, _ := h.svc.Persons().FacesIndexedUpTo()
	return c.JSON(http.StatusOK, map[string]any{
		"persons":          persons,
		"facesIndexedUpTo": indexedUpTo,
	})
}

// GET /v1/photos/persons/:id
func (h *PersonsHandler) Get(c echo.Context) error {
	p, err := h.svc.Persons().GetPerson(c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	rels, err := h.svc.Persons().PersonRelations(p.ID)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"person": p, "relations": rels})
}

// PUT /v1/photos/persons/:id  { name?, favorite?, relation? }
func (h *PersonsHandler) Update(c echo.Context) error {
	var req struct {
		Name     *string `json:"name"`
		Favorite *bool   `json:"favorite"`
		Relation *string `json:"relation"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request body")
	}
	err := h.svc.Persons().UpdatePerson(c.Param("id"),
		service.PersonPatch{Name: req.Name, Favorite: req.Favorite, Relation: req.Relation})
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "updated"})
}

// DELETE /v1/photos/persons/:id  (soft hide)
func (h *PersonsHandler) Delete(c echo.Context) error {
	err := h.svc.Persons().HidePerson(c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "hidden"})
}

// POST /v1/photos/persons/:id/restore
func (h *PersonsHandler) Restore(c echo.Context) error {
	err := h.svc.Persons().RestorePerson(c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "restored"})
}

// GET /v1/photos/persons/:id/assets?limit=&offset=
func (h *PersonsHandler) Assets(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	assets, err := h.svc.Search().PersonAssets(c.Param("id"), limit, offset)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, assets)
}

// GET /v1/photos/persons/:id/relations
func (h *PersonsHandler) Relations(c echo.Context) error {
	rels, err := h.svc.Persons().PersonRelations(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, rels)
}

// GET /v1/photos/persons/:id/places
func (h *PersonsHandler) Places(c echo.Context) error {
	places, err := h.svc.Persons().PersonPlaces(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, places)
}

// GET /v1/photos/persons/:id/face-thumbnail
func (h *PersonsHandler) FaceThumbnail(c echo.Context) error {
	path, err := h.svc.Persons().FaceThumbnail(c.Param("id"), h.faceThumbDir, h.thumbDir)
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.File(path)
}

// POST /v1/photos/persons/:id/detach  { assetIds: [...] }
//
// 把 assetIds 里所有属于该 person 的脸移除，并标记 excluded=1 永久不再聚回。
// 返回 { removed: N }。person 不存在返回 404；空 assetIds 返回 { removed: 0 }。
func (h *PersonsHandler) Detach(c echo.Context) error {
	var req struct {
		AssetIDs []string `json:"assetIds"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	removed, err := h.svc.Persons().DetachAssetsFromPerson(c.Param("id"), req.AssetIDs)
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]int{"removed": removed})
}

// POST /v1/photos/persons/merge  { from_id, into_id }
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

// GET /v1/photos/persons/merge-suggestions
func (h *PersonsHandler) MergeSuggestions(c echo.Context) error {
	sugs, err := h.svc.Persons().MergeSuggestions()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, sugs)
}

// POST /v1/photos/persons/merge-suggestions/reject  { from_id, into_id }
func (h *PersonsHandler) RejectSuggestion(c echo.Context) error {
	var req struct {
		FromID string `json:"from_id"`
		IntoID string `json:"into_id"`
	}
	if err := c.Bind(&req); err != nil || req.FromID == "" || req.IntoID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "from_id and into_id required")
	}
	if err := h.svc.Persons().RejectMerge(req.FromID, req.IntoID); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "rejected"})
}

// POST /v1/photos/persons/recluster
func (h *PersonsHandler) Recluster(c echo.Context) error {
	go h.svc.Faces().RunClustering(h.ctx) //nolint:errcheck
	return c.JSON(http.StatusAccepted, map[string]string{"status": "started"})
}
