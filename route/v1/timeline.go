package v1

import (
	"net/http"

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
