// HTTP route layer for Smart Moments: list/members/pin-to-album/trigger
// recompute/recipe hot-update push endpoint. The data layer (MomentStore)
// and scheduling layer (MomentsService) are in the service package's
// Task 1-4; this file only does param binding, error mapping, and DTO
// conversion — no business logic.
package v1

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// MomentsHandler handles the Smart Moments API.
type MomentsHandler struct {
	svc service.Services
	ctx context.Context // app-lifetime ctx, fed to the background Recompute goroutine — must
	// never use the request's c.Request().Context(), since that ctx gets
	// canceled by net/http once the handler returns and the response is
	// written, which would kill the recompute that was just kicked off
	// (same lesson as PersonsHandler.Recluster).
}

// NewMomentsHandler constructs a MomentsHandler.
func NewMomentsHandler(svc service.Services, ctx context.Context) *MomentsHandler {
	return &MomentsHandler{svc: svc, ctx: ctx}
}

// momentResponse is the external field shape of a single moment for
// GET /v1/photos/moments — snake_case, matching the brief's authoritative
// contract verbatim (unlike the camelCase convention this file otherwise
// follows for its other endpoints).
type momentResponse struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Subtitle     string  `json:"subtitle"`
	CoverAssetID string  `json:"cover_asset_id,omitempty"`
	AssetCount   int     `json:"asset_count"`
	TimeFrom     *string `json:"time_from,omitempty"`
	TimeTo       *string `json:"time_to,omitempty"`
	Place        string  `json:"place,omitempty"`
	RecipeKey    string  `json:"recipe_key"`
	NamedByLLM   bool    `json:"named_by_llm"`
	// SortOrder is the drag-reorder index (nil = not manually ordered); for
	// frontend debugging only, not a hard dependency.
	SortOrder *int `json:"sort_order,omitempty"`
	// FeaturedAssetIDs are the moment's featured member ids (excluding the
	// cover, already truncated to the top maxFeaturedAssetIDsPerMoment by
	// score DESC), letting list pages render a thumbnail strip directly
	// without a per-moment /assets?featured=1 request. Always an array
	// (may be empty []), no omitempty — the frontend doesn't need to
	// null-check this field.
	FeaturedAssetIDs []string `json:"featured_asset_ids"`
	// AddedThisWeek is the count of members added within the current
	// 7-day window (only counted when added_at is non-NULL and falls
	// inside the window; see MomentStore.AddedThisWeekByMoment). Always
	// output (0 included); the frontend only shows the green "+N this
	// week" badge when it's >0.
	AddedThisWeek int `json:"added_this_week"`
	// CoverRatio is the cover's aspect ratio (w/h, see
	// MomentStore.CoverRatioByMoment), used by the frontend to decide
	// landscape/portrait card slots in the mosaic layout. Outputs 0
	// (= unknown) when either dimension is missing or 0 (cover not yet
	// EXIF-indexed, or the field is absent); always output, no
	// omitempty — the frontend doesn't need to null-check this field.
	CoverRatio float64 `json:"cover_ratio"`
}

// maxFeaturedAssetIDsPerMoment is the per-moment cap on featured items when
// List() assembles featured_asset_ids, fed into MomentStore.TopFeaturedByMoment's
// perMoment param. The mosaic layout's three-tile template (T4) consumes up
// to 2 featured items plus a fallback chain, so the cap was raised from 2 to
// 3 (see the 2026-07-29 moments-mosaic design spec, section 1).
const maxFeaturedAssetIDsPerMoment = 3

// toMomentResponse converts service.Moment → momentResponse. When
// TimeFrom/TimeTo are zero-value time.Time (theme moments have no fixed
// time window), the corresponding fields are left empty (nil, omitted from
// JSON); non-zero values are formatted as RFC3339. featuredAssetIDs/
// addedThisWeek/coverRatio are all precomputed by the caller and passed in
// (List() queries TopFeaturedByMoment/AddedThisWeekByMoment/CoverRatioByMoment
// once each for all moments, avoiding per-moment N+1 queries).
func toMomentResponse(m service.Moment, featuredAssetIDs []string, addedThisWeek int, coverRatio float64) momentResponse {
	r := momentResponse{
		ID:               m.ID,
		Title:            m.Title,
		Subtitle:         m.Subtitle,
		CoverAssetID:     m.CoverAssetID,
		AssetCount:       m.AssetCount,
		Place:            m.Place,
		RecipeKey:        m.RecipeKey,
		NamedByLLM:       m.NamedByLLM,
		SortOrder:        m.SortOrder,
		FeaturedAssetIDs: featuredAssetIDs,
		AddedThisWeek:    addedThisWeek,
		CoverRatio:       coverRatio,
	}
	if r.FeaturedAssetIDs == nil {
		r.FeaturedAssetIDs = []string{}
	}
	if !m.TimeFrom.IsZero() {
		s := m.TimeFrom.UTC().Format("2006-01-02T15:04:05Z07:00")
		r.TimeFrom = &s
	}
	if !m.TimeTo.IsZero() {
		s := m.TimeTo.UTC().Format("2006-01-02T15:04:05Z07:00")
		r.TimeTo = &s
	}
	return r
}

// List returns all moments in the shape the brief mandates.
//
// GET /v1/photos/moments
func (h *MomentsHandler) List(c echo.Context) error {
	moments, err := h.svc.Moments().Store().ListMoments()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	// Single query fetches featured_asset_ids for the whole DB (cover
	// excluded from each), dispatched by moment id — never queried
	// per-moment, avoiding N+1 (TopFeaturedByMoment is itself one SQL
	// statement).
	featured, err := h.svc.Moments().Store().TopFeaturedByMoment(maxFeaturedAssetIDsPerMoment)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	// Same approach: one query fetches added_this_week for the whole DB,
	// dispatched by moment id, no N+1.
	addedThisWeek, err := h.svc.Moments().Store().AddedThisWeekByMoment(time.Now().UnixMilli())
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	// Same approach: one query fetches cover aspect ratio for the whole
	// DB, dispatched by moment id, no N+1; ids missing from the map
	// (no exif row, or zero dimensions) get the zero value float64(0),
	// i.e. "unknown".
	coverRatio, err := h.svc.Moments().Store().CoverRatioByMoment()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	out := make([]momentResponse, 0, len(moments))
	for _, m := range moments {
		out = append(out, toMomentResponse(m, featured[m.ID], addedThisWeek[m.ID], coverRatio[m.ID]))
	}
	return c.JSON(http.StatusOK, map[string]any{"moments": out})
}

// findMoment looks up a single moment by id via ListMoments (MomentStore has
// no single-row getter — the store's contract is list + assets + recipes
// only, per the Task 5 brief). Moment counts are small (a handful of active
// trips/themes), so an O(n) scan here is not worth adding a new store method for.
func (h *MomentsHandler) findMoment(id string) (service.Moment, error) {
	moments, err := h.svc.Moments().Store().ListMoments()
	if err != nil {
		return service.Moment{}, err
	}
	for _, m := range moments {
		if m.ID == id {
			return m, nil
		}
	}
	return service.Moment{}, service.ErrNotFound
}

// momentAssetMemberDTO is the member metadata attached when with_members=1,
// letting the edit UI distinguish members "produced by this engine run"
// from those "manually pinned by the user", and whether each is featured.
type momentAssetMemberDTO struct {
	AssetID  string `json:"asset_id"`
	Manual   bool   `json:"manual"`
	Featured bool   `json:"featured"`
}

// momentPlaceDTO is the About-section multi-place aggregate data attached
// when with_members=1 (see design spec section 3), converted from
// service.MomentPlace.
type momentPlaceDTO struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// maxPlacesPerMoment caps the number of cities in Assets(with_members=1)'s
// places field, preventing moments with an extreme number of places from
// bloating the response (see design spec section 3).
const maxPlacesPerMoment = 8

// Assets returns a moment's member assets, serialized via the same asset
// shape as GET /v1/photos/assets. Query param featured=1 restricts to the
// featured subset; members are already ordered by score DESC by
// MomentStore.GetMomentAssets, and that order is preserved end-to-end because
// Search().ListAssets short-circuits on a non-nil AssetIDs filter without
// re-sorting.
//
// With with_members=1, the response shape becomes {"assets":[...], "members":
// [{"asset_id","manual","featured"}]}, letting the edit UI get each member's
// origin/featured flag in the same call; without that param, the response
// stays the existing bare array unchanged (during the deploy window, old
// frontends still parse it as a bare array — see the brief's ambiguity
// ruling).
//
// GET /v1/photos/moments/:id/assets?featured=1&with_members=1
func (h *MomentsHandler) Assets(c echo.Context) error {
	id := c.Param("id")
	if _, err := h.findMoment(id); err != nil {
		return mapMomentErr(err)
	}

	featuredOnly := c.QueryParam("featured") == "1"
	members, err := h.svc.Moments().Store().GetMomentAssets(id, featuredOnly)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.AssetID)
	}
	assets, err := h.svc.Search().ListAssets(JWTUserID(c), 0, 0, service.AssetFilter{AssetIDs: ids})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	if c.QueryParam("with_members") != "1" {
		return c.JSON(http.StatusOK, assets)
	}

	memberDTOs := make([]momentAssetMemberDTO, 0, len(members))
	for _, m := range members {
		memberDTOs = append(memberDTOs, momentAssetMemberDTO{
			AssetID:  m.AssetID,
			Manual:   m.Manual,
			Featured: m.Featured,
		})
	}

	places, err := h.svc.Moments().Store().PlacesByMoment(id, maxPlacesPerMoment)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	placeDTOs := make([]momentPlaceDTO, 0, len(places))
	for _, p := range places {
		placeDTOs = append(placeDTOs, momentPlaceDTO{Name: p.Name, Count: p.Count})
	}

	return c.JSON(http.StatusOK, map[string]any{"assets": assets, "members": memberDTOs, "places": placeDTOs})
}

// momentAssetsIDsRequest is the request body shape shared by
// Pin/ExcludeAssets: a batch of asset ids to operate on.
type momentAssetsIDsRequest struct {
	IDs []string `json:"ids"`
}

// PinAssets forcibly merges some assets into a moment: records one edit and
// updates membership immediately (see MomentStore.PinMomentAssets), which
// also takes effect on the next engine recompute (replay hook in
// momentstore.go applyMomentEdits). Empty ids is treated as an invalid
// request → 400; a moment that doesn't exist or is hidden → 404 (findMoment
// catches this uniformly; ListMoments already filters out hidden, so this
// path never reaches the store's error path for an unknown momentID). Ids
// not present in the assets table are silently ignored by the store layer
// and don't affect the success of this request.
//
// POST /v1/photos/moments/:id/assets
//
//	{ "ids": ["assetId1", "assetId2", ...] }
func (h *MomentsHandler) PinAssets(c echo.Context) error {
	id := c.Param("id")
	if _, err := h.findMoment(id); err != nil {
		return mapMomentErr(err)
	}
	var req momentAssetsIDsRequest
	if err := c.Bind(&req); err != nil || len(req.IDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "ids is required")
	}
	count, err := h.svc.Moments().Store().PinMomentAssets(id, req.IDs)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "asset_count": count})
}

// ExcludeAssets forcibly removes some assets from a moment (see
// MomentStore.ExcludeMomentAssets); cover reselection is already done at
// the store layer. Empty ids / moment not found or hidden are handled
// identically to PinAssets.
//
// DELETE /v1/photos/moments/:id/assets
//
//	{ "ids": ["assetId1", "assetId2", ...] }
func (h *MomentsHandler) ExcludeAssets(c echo.Context) error {
	id := c.Param("id")
	if _, err := h.findMoment(id); err != nil {
		return mapMomentErr(err)
	}
	var req momentAssetsIDsRequest
	if err := c.Bind(&req); err != nil || len(req.IDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "ids is required")
	}
	count, err := h.svc.Moments().Store().ExcludeMomentAssets(id, req.IDs)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "asset_count": count})
}

// Delete hides a moment (tombstone; the row itself is kept, see
// MomentStore.HideMoment). Moment not found or already hidden → 404
// (findMoment is based on ListMoments, which already filters hidden=0, so
// repeated DELETEs of the same id are also 404 from the second call on —
// this is the expected idempotent behavior). Once hidden takes effect,
// List/Assets/CreateAlbum (export) all return 404 / stop listing it.
//
// DELETE /v1/photos/moments/:id
func (h *MomentsHandler) Delete(c echo.Context) error {
	id := c.Param("id")
	if _, err := h.findMoment(id); err != nil {
		return mapMomentErr(err)
	}
	if err := h.svc.Moments().Store().HideMoment(id); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true})
}

// CreateAlbum pins a moment into a regular album (all members, not just
// featured), using the moment's title as the album name — delegating to
// the existing AlbumService.Create + BatchAddAssets (the same thin
// wrapper pattern as smartview.go's ExportAsAlbum, but written directly in
// the handler here without touching smartview.go).
//
// POST /v1/photos/moments/:id/album
func (h *MomentsHandler) CreateAlbum(c echo.Context) error {
	id := c.Param("id")
	moment, err := h.findMoment(id)
	if err != nil {
		return mapMomentErr(err)
	}

	members, err := h.svc.Moments().Store().GetMomentAssets(id, false)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.AssetID)
	}

	album, err := h.svc.Albums().Create(moment.Title)
	if err != nil {
		return mapAlbumErr(err)
	}
	if len(ids) > 0 {
		if err := h.svc.Albums().BatchAddAssets(album.ID, ids); err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
	}
	return c.JSON(http.StatusCreated, map[string]any{
		"albumId": album.ID,
		"name":    moment.Title,
		"count":   len(ids),
	})
}

// Recompute asynchronously triggers a full recompute (RecomputeAll has its
// own CAS reentrancy guard and just returns nil on reentry); it doesn't
// block the request. Progress goes through the existing TaskRegistry
// (Type: "moments"); this just replies with a 202 + task_type so the
// frontend knows which task to poll.
//
// POST /v1/photos/moments/recompute
func (h *MomentsHandler) Recompute(c echo.Context) error {
	go func() {
		if err := h.svc.Moments().RecomputeAll(h.ctx); err != nil {
			zap.L().Warn("moments: manually triggered recompute failed", zap.Error(err))
		}
	}()
	return c.JSON(http.StatusAccepted, map[string]string{"task_type": "moments"})
}

// recipeDTO is the external shape of the recipe hot-update endpoint: Params
// is expanded into the parsed RecipeParams (rather than the raw JSON string
// stored internally); the GET side fills in defaults via ParseParams, and
// the PUT side serializes it back into ParamsJSON for storage.
type recipeDTO struct {
	Key       string               `json:"key"`
	Kind      string               `json:"kind"`
	Title     string               `json:"title"`
	Params    service.RecipeParams `json:"params"`
	Enabled   bool                 `json:"enabled"`
	UpdatedAt int64                `json:"updated_at,omitempty"`
}

// ListRecipes lists all recipes (including disabled ones), for the recipe
// management UI to display/edit.
//
// GET /v1/photos/moments/recipes
func (h *MomentsHandler) ListRecipes(c echo.Context) error {
	recipes, err := h.svc.Moments().Store().ListRecipes(false)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	out := make([]recipeDTO, 0, len(recipes))
	for _, r := range recipes {
		params, perr := service.ParseParams(r)
		if perr != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, perr.Error())
		}
		out = append(out, recipeDTO{
			Key: r.Key, Kind: r.Kind, Title: r.Title,
			Params: params, Enabled: r.Enabled, UpdatedAt: r.UpdatedAt,
		})
	}
	return c.JSON(http.StatusOK, map[string]any{"recipes": out})
}

// UpdateRecipes is the recipe hot-update push endpoint: a batch upsert
// (keyed by key) that takes effect on the next RecomputeAll, no code
// changes or service restart needed.
//
// PUT /v1/photos/moments/recipes
//
//	{ "recipes": [{ "key", "kind", "title", "params", "enabled" }, ...] }
func (h *MomentsHandler) UpdateRecipes(c echo.Context) error {
	var req struct {
		Recipes []recipeDTO `json:"recipes"`
	}
	if err := c.Bind(&req); err != nil || len(req.Recipes) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "recipes is required")
	}

	recipes := make([]service.MomentRecipe, 0, len(req.Recipes))
	for _, dto := range req.Recipes {
		if dto.Key == "" || dto.Kind == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "recipe key/kind is required")
		}
		if dto.Kind != "trip" && dto.Kind != "theme" {
			// kind whitelist: the engine (service/moments.go recomputeRecipe)
			// only recognizes "trip"/"theme"; any other kind is silently
			// skipped with a Warn (no error) — better to catch it at this
			// push endpoint layer instead: it prevents an ops/script typo
			// (e.g. "thmee"/"Trip") from quietly writing an invalid recipe
			// into the DB, where recompute would never produce anything and
			// there'd be no error to notice.
			return echo.NewHTTPError(http.StatusBadRequest, "kind must be one of: trip, theme")
		}
		paramsJSON, err := json.Marshal(dto.Params)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		recipes = append(recipes, service.MomentRecipe{
			Key:        dto.Key,
			Kind:       dto.Kind,
			Title:      dto.Title,
			ParamsJSON: string(paramsJSON),
			Enabled:    dto.Enabled,
		})
	}

	if err := h.svc.Moments().Store().UpsertRecipes(recipes); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"updated": len(recipes)})
}

// ReorderMoments is the persistence endpoint for drag reordering: the ids
// order in the body is the user's post-drag display order, and sort_order
// is assigned accordingly within a transaction (see
// MomentStore.ReorderMoments). Empty ids is treated as an invalid request
// (a drag always carries at least one id) → 400; unknown ids in the body
// (the frontend's list may be slightly stale) are ignored at the store
// layer and don't affect the success of this request.
//
// PUT /v1/photos/moments/order
//
//	{ "ids": ["id1", "id2", ...] }
func (h *MomentsHandler) ReorderMoments(c echo.Context) error {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := c.Bind(&req); err != nil || len(req.IDs) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "ids is required")
	}
	if err := h.svc.Moments().Store().ReorderMoments(req.IDs); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true})
}

// mapMomentErr maps moment-lookup errors to HTTP responses.
func mapMomentErr(err error) error {
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
}
