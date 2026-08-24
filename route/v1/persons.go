package v1

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// PersonsHandler provides People-related HTTP endpoints.
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

// PUT /v1/photos/persons/:id  { name?, favorite?, relation?, heroAssetId? }
func (h *PersonsHandler) Update(c echo.Context) error {
	var req struct {
		Name        *string `json:"name"`
		Favorite    *bool   `json:"favorite"`
		Relation    *string `json:"relation"`
		HeroAssetID *string `json:"heroAssetId"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request body")
	}
	err := h.svc.Persons().UpdatePerson(c.Param("id"),
		service.PersonPatch{Name: req.Name, Favorite: req.Favorite, Relation: req.Relation, HeroAssetID: req.HeroAssetID})
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "updated"})
}

// PUT /v1/photos/persons/:id/cover  { assetId: "..." }
// Pins the cover face to the best face found on the given asset and sets cover_locked=1.
func (h *PersonsHandler) SetCover(c echo.Context) error {
	var req struct {
		AssetID string `json:"assetId"`
	}
	if err := c.Bind(&req); err != nil || req.AssetID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "assetId required")
	}
	faceID, err := h.svc.Persons().SetPersonCover(c.Param("id"), req.AssetID)
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "ok", "coverFaceId": faceID})
}

// DELETE /v1/photos/persons/:id/cover
// Clears cover_locked and recomputes cover by centroid distance.
func (h *PersonsHandler) DeleteCover(c echo.Context) error {
	faceID, err := h.svc.Persons().UnlockPersonCover(c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "ok", "coverFaceId": faceID})
}

// DELETE /v1/photos/persons/:id
//
// ?purge=true  → permanently delete: exclude faces, drop bindings, delete person row.
// (default)    → undoable soft hide: sets hidden=1 and schedules a hard purge
//
//	after the grace period unless restored first.
func (h *PersonsHandler) Delete(c echo.Context) error {
	id := c.Param("id")
	if c.QueryParam("purge") == "true" {
		err := h.svc.Persons().PurgePerson(id)
		if errors.Is(err, service.ErrNotFound) {
			return echo.NewHTTPError(http.StatusNotFound)
		}
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, map[string]string{"status": "purged"})
	}
	err := h.svc.Persons().HidePersonForPurge(id)
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "hidden"})
}

// POST /v1/photos/persons/:id/hide
//
// Plain hide: sets hidden=1 with no purge_at, so PurgeDuePersons' sweep
// (which only touches purge_at IS NOT NULL rows) never picks this up. This
// is distinct from Delete's default path (HidePersonForPurge), which
// schedules a hard purge after the grace period.
func (h *PersonsHandler) Hide(c echo.Context) error {
	err := h.svc.Persons().HidePerson(c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "hidden"})
}

// GET /v1/photos/persons/hidden
//
// Lists plainly-hidden persons for the hidden-people management view.
// Excludes persons currently in the purge grace period (those are "being
// deleted", not "hidden"). Must be registered before GET /persons/:id or
// "hidden" would be swallowed as an :id lookup — see route/router.go.
func (h *PersonsHandler) ListHidden(c echo.Context) error {
	persons, err := h.svc.Persons().ListHiddenPersons()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, persons)
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
	if err := h.svc.Persons().PersonVisible(c.Param("id")); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return echo.NewHTTPError(http.StatusNotFound)
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
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
	if err := h.svc.Persons().PersonVisible(c.Param("id")); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return echo.NewHTTPError(http.StatusNotFound)
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	rels, err := h.svc.Persons().PersonRelations(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, rels)
}

// GET /v1/photos/persons/:id/places
func (h *PersonsHandler) Places(c echo.Context) error {
	if err := h.svc.Persons().PersonVisible(c.Param("id")); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return echo.NewHTTPError(http.StatusNotFound)
		}
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
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

// GET /v1/photos/faces/:id/thumbnail
//
// Serves a cropped, cached square thumbnail for an arbitrary face_detections
// id, independent of person membership — a suggestions-inbox candidate face
// may be a free-floating face not (yet, or no longer) attached to any
// person. JWT-exempt via the generic "*/thumbnail" suffix rule in
// mediaGetSkip (route/router.go) — no separate rule needed, <img> tags can't
// attach an Authorization header. 404s for an unknown face id or an
// offline/deleted owning asset (see service.PersonService.FaceThumbnailByID).
func (h *PersonsHandler) FaceThumbnailByID(c echo.Context) error {
	path, err := h.svc.Persons().FaceThumbnailByID(c.Param("id"), h.faceThumbDir, h.thumbDir)
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
// Removes all faces belonging to this person among assetIds, and marks them excluded=1 so they never re-cluster back.
// Returns { removed: N }. Returns 404 if the person doesn't exist; returns { removed: 0 } for an empty assetIds.
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
	if req.FromID == req.IntoID {
		return echo.NewHTTPError(http.StatusBadRequest, "from_id and into_id must differ")
	}
	if err := h.svc.Search().MergePersons(req.FromID, req.IntoID); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return echo.NewHTTPError(http.StatusNotFound)
		}
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

// ── Cluster-merge questions API (durable gray-band merge suggestions) ─────
// Distinct from MergeSuggestions/RejectSuggestion above (the legacy
// on-the-fly, named-centroid-only computation) and from the
// PersonSuggestion(s) join/review queue below: this is the apple engine's
// HAC-gray-band queue, generated durably during RunClustering and persisted
// in merge_suggestions/face_negative_pairs. See service/merge_questions.go
// and OVERVIEW.md's "Cluster-merge questions" section.

// GET /v1/photos/persons/merge-suggestions/v2
//
// Lists every open cluster-merge question (dist ascending), hidden persons
// excluded on either side. Must be registered before GET /persons/:id in
// route/router.go, same route-order trap as /persons/hidden and
// /persons/suggestions.
func (h *PersonsHandler) MergeQuestions(c echo.Context) error {
	pairs, err := h.svc.Persons().ListMergeQuestions()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"pairs": pairs})
}

// POST /v1/photos/persons/merge-suggestions/v2/:id/accept
func (h *PersonsHandler) AcceptMergeQuestion(c echo.Context) error {
	dec, err := h.svc.Persons().AcceptMergeSuggestion(c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, dec)
}

// POST /v1/photos/persons/merge-suggestions/v2/:id/reject
func (h *PersonsHandler) RejectMergeQuestion(c echo.Context) error {
	dec, err := h.svc.Persons().RejectMergeSuggestion(c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, dec)
}

// POST /v1/photos/persons/recluster
func (h *PersonsHandler) Recluster(c echo.Context) error {
	go h.svc.Faces().RunClustering(h.ctx) //nolint:errcheck
	return c.JSON(http.StatusAccepted, map[string]string{"status": "started"})
}

// ── Person suggestions API (Task 6 of the exemplar-assignment plan) ───────
// Distinct from MergeSuggestions/RejectSuggestion above (a different table
// and workflow: merge-candidate pairs, not join/review gray-zone faces).
// Handler names below carry the "PersonSuggestion(s)" qualifier specifically
// to avoid colliding with (and being confused for) that older concept.

// GET /v1/photos/persons/suggestions
//
// Lists every open join/review suggestion, grouped by person (hidden
// persons' suggestions excluded). Must be registered before GET
// /persons/:id in route/router.go, or "suggestions" would be swallowed as an
// :id lookup — same route-order trap as /persons/hidden.
func (h *PersonsHandler) PersonSuggestions(c echo.Context) error {
	groups, err := h.svc.Persons().ListSuggestions()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"groups": groups})
}

// POST /v1/photos/persons/suggestions/:id/accept
func (h *PersonsHandler) AcceptPersonSuggestion(c echo.Context) error {
	dec, err := h.svc.Persons().AcceptSuggestion(c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, dec)
}

// POST /v1/photos/persons/suggestions/:id/reject
func (h *PersonsHandler) RejectPersonSuggestion(c echo.Context) error {
	dec, err := h.svc.Persons().RejectSuggestion(c.Param("id"))
	if errors.Is(err, service.ErrNotFound) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, dec)
}

// suggestionBatchResult is one id's outcome within BatchPersonSuggestions'
// response map: "accepted"/"rejected" on success, "error" (with a message)
// when the id was unknown or belonged to a hidden person.
type suggestionBatchResult struct {
	Status    string     `json:"status"`
	DecidedAt *time.Time `json:"decidedAt,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// ── Threshold self-calibration API (Task 9) ───────────────────────────────
// Status/history/factory-profile hot-update for the self-calibration
// runner (service/calibrate_run.go). All three back onto *FaceService
// (service/calibrate_api.go), never a second implementation of the
// resolver/truth-loading logic those files already own.

// GET /v1/photos/persons/calibration
//
// Must be registered before GET /persons/:id in route/router.go, same
// route-order trap as /persons/hidden and /persons/suggestions above --
// otherwise "calibration" would be swallowed as an :id lookup.
func (h *PersonsHandler) GetCalibration(c echo.Context) error {
	status, err := h.svc.Faces().CalibrationStatus()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, status)
}

// GET /v1/photos/persons/calibration/history?limit=
//
// Must precede GET /persons/:id for the same route-order reason as
// /persons/calibration above.
func (h *PersonsHandler) CalibrationHistory(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	runs, err := h.svc.Faces().CalibrationHistory(limit)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"runs": runs})
}

// PUT /v1/photos/persons/calibration/profile
//
// Factory-profile hot-update: the request body is the complete profile
// document, validated and persisted by UpdateCalibrationProfile. Unlike the
// two read-only endpoints above, this write can move every self-calibration
// safety rail, so it must NEVER be added to route/router.go's JWT-exempt
// mediaGetSkip suffix list -- it is deliberately absent from it (verified in
// route/v1/persons_calibration_test.go and route/router.go's own route-order
// comment at registration).
func (h *PersonsHandler) PutCalibrationProfile(c echo.Context) error {
	raw, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to read request body")
	}
	version, err := h.svc.Faces().UpdateCalibrationProfile(raw)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return c.JSON(http.StatusOK, map[string]int{"version": version})
}

// POST /v1/photos/persons/suggestions/batch  { "accept": [ids], "reject": [ids] }
//
// Response: { "results": { "<id>": {"status": "accepted"|"rejected"|"error",
// "decidedAt"?, "error"?} } }. Every id is processed independently (each
// accept/reject call is its own transaction, per decideSuggestion — one id's
// failure never rolls back another's decision) and the endpoint always
// returns 200 with the per-id result map, so the caller can render a
// partial-success summary instead of treating the whole batch as failed. An
// id present in both arrays is processed accept-then-reject in that order,
// so the reject outcome wins that id's slot in the result map.
func (h *PersonsHandler) BatchPersonSuggestions(c echo.Context) error {
	var req struct {
		Accept []string `json:"accept"`
		Reject []string `json:"reject"`
	}
	if err := c.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request body")
	}
	results := map[string]suggestionBatchResult{}
	decide := func(id string, accept bool) {
		var dec *service.SuggestionDecision
		var err error
		if accept {
			dec, err = h.svc.Persons().AcceptSuggestion(id)
		} else {
			dec, err = h.svc.Persons().RejectSuggestion(id)
		}
		switch {
		case errors.Is(err, service.ErrNotFound):
			results[id] = suggestionBatchResult{Status: "error", Error: "not found"}
		case err != nil:
			results[id] = suggestionBatchResult{Status: "error", Error: err.Error()}
		default:
			results[id] = suggestionBatchResult{Status: dec.Status, DecidedAt: dec.DecidedAt}
		}
	}
	for _, id := range req.Accept {
		decide(id, true)
	}
	for _, id := range req.Reject {
		decide(id, false)
	}
	return c.JSON(http.StatusOK, map[string]any{"results": results})
}
