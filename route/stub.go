// Package route registers HTTP handlers for NimoOS-Photos.
// This file is a stub; full implementation is added in subsequent tasks.
package route

import (
	"net/http"

	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// InitRouter sets up the Echo router and returns an http.Handler.
// The runtimePath parameter is reserved for future middleware use.
func InitRouter(svc service.Services, runtimePath string) http.Handler {
	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.Recover())
	e.GET("/healthz", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})
	return e
}
