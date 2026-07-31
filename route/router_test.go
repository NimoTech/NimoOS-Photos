package route

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/require"
)

func TestMcpReadSkip(t *testing.T) {
	base := common.V1APIPath
	cases := []struct {
		name                     string
		method, path, ip, userID string
		want                     bool
	}{
		{"albums GET localhost with uid", http.MethodGet, base + "/albums", "127.0.0.1", "42", true},
		{"albums GET localhost no uid (fail-closed)", http.MethodGet, base + "/albums", "127.0.0.1", "", false},
		{"albums POST localhost (write stays protected)", http.MethodPost, base + "/albums", "127.0.0.1", "42", false},
		{"albums GET external ip", http.MethodGet, base + "/albums", "192.168.1.5", "42", false},
		{"search/smart POST localhost with uid", http.MethodPost, base + "/search/smart", "127.0.0.1", "42", true},
		{"suffix-lookalike not exact-matched", http.MethodGet, base + "/public/albums", "127.0.0.1", "42", false},
	}
	for _, c := range cases {
		if got := mcpReadSkip(c.method, c.path, c.ip, c.userID); got != c.want {
			t.Errorf("%s: mcpReadSkip=%v want %v", c.name, got, c.want)
		}
	}
}

// TestMediaGetSkip 是 mediaGetSkip 的单元测试：thumbnail/preview/sprite 等
// 后缀白名单必须限定 GET，POST 命中同后缀（如 /smart-views/preview）不得放行。
func TestMediaGetSkip(t *testing.T) {
	base := common.V1APIPath
	cases := []struct {
		name         string
		method, path string
		want         bool
	}{
		{"GET assets/:id/preview allowed", http.MethodGet, base + "/assets/:id/preview", true},
		{"GET assets/:id/sprite allowed", http.MethodGet, base + "/assets/:id/sprite", true},
		{"GET assets/:id/thumbnail allowed", http.MethodGet, base + "/assets/:id/thumbnail", true},
		{"GET favorites/export allowed", http.MethodGet, base + "/favorites/export", true},
		{"GET albums/:id/export allowed", http.MethodGet, base + "/albums/:id/export", true},
		{"POST albums/:id/export NOT allowed (方法前置校验)", http.MethodPost, base + "/albums/:id/export", false},
		{"POST smart-views/preview NOT allowed (碰撞回归)", http.MethodPost, base + "/smart-views/preview", false},
		{"POST assets/:id/preview (若存在) NOT allowed", http.MethodPost, base + "/assets/:id/preview", false},
		{"PUT assets/:id/sprite (若存在) NOT allowed", http.MethodPut, base + "/assets/:id/sprite", false},
	}
	for _, c := range cases {
		if got := mediaGetSkip(c.method, c.path); got != c.want {
			t.Errorf("%s: mediaGetSkip=%v want %v", c.name, got, c.want)
		}
	}
}

// newTestJWTRouter 搭建一个与 InitRouter 中 JWT 中间件完全同构（同一份
// Skipper 逻辑：mediaGetSkip + mcpReadSkip）的最小 Echo 实例，并注册与生产
// 环境路径前缀相同、且存在"POST /smart-views/preview 与 GET */preview"
// 后缀碰撞风险的路由，用来在真实 HTTP 请求层面（而非仅调用裸函数）复现/回
// 归验证审查者报告的鉴权绕过问题。ParseTokenFunc 恒失败，模拟"未带/带无效
// JWT"的请求 —— 只要 Skipper 没有误放行，中间件必然返回 401。
func newTestJWTRouter() *echo.Echo {
	e := echo.New()
	e.Use(echo_middleware.JWTWithConfig(echo_middleware.JWTConfig{
		Skipper: func(c echo.Context) bool {
			if c.Request().Method == http.MethodOptions {
				return true
			}
			p := c.Path()
			if p == common.V1APIPath+"/version" {
				return true
			}
			if mediaGetSkip(c.Request().Method, p) {
				return true
			}
			return mcpReadSkip(c.Request().Method, p, c.RealIP(),
				c.Request().Header.Get("X-NimoOS-User-ID"))
		},
		ParseTokenFunc: func(token string, c echo.Context) (interface{}, error) {
			return nil, echo.ErrUnauthorized
		},
		// 与生产配置（router.go）保持一致：自定义提取器只做 TrimPrefix，
		// 缺失 Authorization 头时返回空串而不报错，交由 ParseTokenFunc 判定失败。
		// 若省略此项，Echo 会退回默认 header 提取器，请求缺头时会在提取阶段
		// 就以 400（而非生产实际会看到的 401）短路，掩盖真实行为。
		TokenLookupFuncs: []echo_middleware.ValuesExtractor{
			func(c echo.Context) ([]string, error) {
				auth := c.Request().Header.Get(echo.HeaderAuthorization)
				return []string{strings.TrimPrefix(auth, "Bearer ")}, nil
			},
		},
	}))

	ok := func(c echo.Context) error { return c.String(http.StatusOK, "ok") }

	g := e.Group(common.V1APIPath)
	g.POST("/smart-views/preview", ok)
	g.GET("/assets/:id/preview", ok)
	g.GET("/assets/:id/sprite", ok)
	return e
}

// TestJWTExemption_PreviewSuffixCollision 是 route 层回归测试：
//  1. 无 Authorization 头的 POST /smart-views/preview 必须被 JWT 中间件拒绝
//     （审查者复现的鉴权绕过：修复前该请求会被 "/preview" 后缀白名单误放行，
//     返回 200；本用例在修复前会 FAIL，修复后 PASS）。
//  2. 无 Authorization 头的 GET /assets/:id/preview、GET /assets/:id/sprite
//     仍应正常放行进入 handler（媒体豁免不能被误伤）。
func TestJWTExemption_PreviewSuffixCollision(t *testing.T) {
	e := newTestJWTRouter()

	post := httptest.NewRequest(http.MethodPost, common.V1APIPath+"/smart-views/preview", nil)
	postRec := httptest.NewRecorder()
	e.ServeHTTP(postRec, post)
	require.Equal(t, http.StatusUnauthorized, postRec.Code,
		"POST /smart-views/preview 不应被 /preview 后缀白名单误放行（鉴权绕过回归）")

	getPreview := httptest.NewRequest(http.MethodGet, common.V1APIPath+"/assets/abc/preview", nil)
	getPreviewRec := httptest.NewRecorder()
	e.ServeHTTP(getPreviewRec, getPreview)
	require.Equal(t, http.StatusOK, getPreviewRec.Code,
		"GET /assets/:id/preview 应保持 JWT 豁免、正常进入 handler")

	getSprite := httptest.NewRequest(http.MethodGet, common.V1APIPath+"/assets/abc/sprite", nil)
	getSpriteRec := httptest.NewRecorder()
	e.ServeHTTP(getSpriteRec, getSprite)
	require.Equal(t, http.StatusOK, getSpriteRec.Code,
		"GET /assets/:id/sprite 应保持 JWT 豁免、正常进入 handler")
}
