package v1

import (
	"net/http"
	"strconv"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
)

// TimelineHandler serves chronologically grouped asset timelines.
type TimelineHandler struct{ svc service.Services }

// NewTimelineHandler constructs a TimelineHandler.
func NewTimelineHandler(svc service.Services) *TimelineHandler { return &TimelineHandler{svc} }

// List returns all non-live-photo-video assets grouped by year/month in
// descending chronological order.
//
// GET /v1/photos/timeline
func (h *TimelineHandler) List(c echo.Context) error {
	groups, err := h.svc.Search().Timeline(JWTUserID(c))
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, groups)
}

// Buckets returns the per-month bucket directory of the timeline.
//
// GET /v1/photos/timeline/buckets
func (h *TimelineHandler) Buckets(c echo.Context) error {
	buckets, err := h.svc.Search().TimelineBuckets()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, buckets)
}

// Bucket returns one month bucket's assets, paginated.
//
// GET /v1/photos/timeline/bucket?year=&month=&limit=&offset=
func (h *TimelineHandler) Bucket(c echo.Context) error {
	year, _ := strconv.Atoi(c.QueryParam("year"))
	month, _ := strconv.Atoi(c.QueryParam("month"))
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	if month < 0 || month > 12 || year < 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid year/month")
	}
	if !validBucketYM(year, month) {
		return echo.NewHTTPError(http.StatusBadRequest, "year and month must both be zero (unknown bucket) or both be set")
	}
	assets, err := h.svc.Search().TimelineBucketAssets(JWTUserID(c), year, month, limit, offset)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if assets == nil {
		assets = []service.Asset{}
	}
	return c.JSON(http.StatusOK, assets)
}

// validBucketYM reports whether year/month is a valid timeline bucket key:
// either both zero (the "unknown date" bucket — taken_at and indexed_at both
// NULL) or both set with month in 1..12. A "half-zero" pair like
// year=2020,month=0 can never correspond to a real bucket. Callers are
// expected to have already rejected out-of-range values (negative
// year/month, month>12) before consulting this half-zero rule.
func validBucketYM(year, month int) bool {
	if year < 0 || month < 0 || month > 12 {
		return false
	}
	return (year == 0) == (month == 0)
}
