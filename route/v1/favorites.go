package v1

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

type FavoritesHandler struct {
	svc        service.Services
	galleryDir string
}

func NewFavoritesHandler(svc service.Services, galleryDir string) *FavoritesHandler {
	return &FavoritesHandler{svc: svc, galleryDir: galleryDir}
}

func (h *FavoritesHandler) Favorite(c echo.Context) error {
	favAt, err := h.svc.Favorites().Favorite(JWTUserID(c), c.Param("asset_id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound, "asset not found")
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"favoritedAt": favAt,
	})
}

func (h *FavoritesHandler) Unfavorite(c echo.Context) error {
	if err := h.svc.Favorites().Unfavorite(JWTUserID(c), c.Param("asset_id")); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *FavoritesHandler) ListIDs(c echo.Context) error {
	ids, err := h.svc.Favorites().ListIDs(JWTUserID(c))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if ids == nil {
		ids = []string{}
	}
	return c.JSON(http.StatusOK, ids)
}

func (h *FavoritesHandler) List(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if limit < 0 || limit > 500 {
		limit = 0
	}
	assets, err := h.svc.Favorites().List(JWTUserID(c), service.ListFavoritesOpts{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if assets == nil {
		assets = []service.Asset{}
	}
	return c.JSON(http.StatusOK, assets)
}
