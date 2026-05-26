package v1

import "github.com/labstack/echo/v4"

// JWTUserID returns the JWT subject the router middleware set on the request
// header. Falls back to "default" when absent (used in tests, internal calls,
// or pre-multi-user state).
func JWTUserID(c echo.Context) string {
	uid := c.Request().Header.Get("X-NimoOS-User-ID")
	if uid == "" {
		return "default"
	}
	return uid
}
