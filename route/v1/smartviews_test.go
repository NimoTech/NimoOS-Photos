package v1_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	v1 "github.com/NimoTech/NimoOS-Photos/route/v1"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
)

func newTestEcho(t *testing.T) *echo.Echo {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "h.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc := service.NewTestServices(db)
	e := echo.New()
	g := e.Group("/v1/photos")
	h := v1.NewSmartViewsHandler(svc, "")
	v1.RegisterSmartViewRoutes(g, h)
	return e
}

// newAssetsTestEcho 同 newTestEcho,但额外返回底层 db,供钉住/移除/恢复/排除清单
// 用例直接插入 assets / smart_views / smart_view_matches 造数据。
func newAssetsTestEcho(t *testing.T) (*echo.Echo, *sql.DB) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "h.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	svc := service.NewTestServices(db)
	e := echo.New()
	g := e.Group("/v1/photos")
	h := v1.NewSmartViewsHandler(svc, "")
	v1.RegisterSmartViewRoutes(g, h)
	return e, db
}

// postAssetIDs 向指定路径 POST {"assetIds":ids} 并返回响应。
func postAssetIDs(t *testing.T, e *echo.Echo, path string, ids []string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"assetIds": ids})
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// TestPinAssetsHTTPNotFound 视图不存在应返回 404。
func TestPinAssetsHTTPNotFound(t *testing.T) {
	e, _ := newAssetsTestEcho(t)
	rec := postAssetIDs(t, e, "/v1/photos/smart-views/sv-missing/assets", []string{"a1"})
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestPinAssetsHTTPEmptyBody assetIds 为空应返回 400。
func TestPinAssetsHTTPEmptyBody(t *testing.T) {
	e, db := newAssetsTestEcho(t)
	_, err := db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live) VALUES('sv-1','V','[]','[]',70,0)`)
	require.NoError(t, err)

	rec := postAssetIDs(t, e, "/v1/photos/smart-views/sv-1/assets", []string{})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestPinAssetsHTTPSuccess 正常路径:钉住一个有效资产,返回 added 计数。
func TestPinAssetsHTTPSuccess(t *testing.T) {
	e, db := newAssetsTestEcho(t)
	_, err := db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live) VALUES('sv-1','V','[]','[]',70,0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('a1','/p/a1.jpg','indexed')`)
	require.NoError(t, err)

	rec := postAssetIDs(t, e, "/v1/photos/smart-views/sv-1/assets", []string{"a1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]int
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 1, resp["added"])

	var org int
	require.NoError(t, db.QueryRow(`SELECT origin FROM smart_view_matches WHERE smart_view_id='sv-1' AND asset_id='a1'`).Scan(&org))
	require.Equal(t, 1, org)
}

// TestRemoveAssetsHTTPSuccess 正常路径:一个钉住行 + 一个自动行,分别计 unpinned/excluded。
func TestRemoveAssetsHTTPSuccess(t *testing.T) {
	e, db := newAssetsTestEcho(t)
	_, err := db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live) VALUES('sv-1','V','[]','[]',70,0)`)
	require.NoError(t, err)
	for _, id := range []string{"aPin", "aAuto"} {
		_, err = db.Exec(`INSERT INTO assets(id,file_path,status) VALUES(?,?,'indexed')`, id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-1','aPin',1.0,1)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-1','aAuto',0.6,0)`)
	require.NoError(t, err)

	rec := postAssetIDs(t, e, "/v1/photos/smart-views/sv-1/assets/remove", []string{"aPin", "aAuto"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]int
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 1, resp["unpinned"])
	require.Equal(t, 1, resp["excluded"])
}

// TestRestoreAssetsHTTPSuccess 正常路径:恢复一个排除行,返回 restored 计数。
func TestRestoreAssetsHTTPSuccess(t *testing.T) {
	e, db := newAssetsTestEcho(t)
	_, err := db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live) VALUES('sv-1','V','[]','[]',70,0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id,file_path,status) VALUES('aExcl','/p/aExcl.jpg','indexed')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-1','aExcl',0.6,2)`)
	require.NoError(t, err)

	rec := postAssetIDs(t, e, "/v1/photos/smart-views/sv-1/assets/restore", []string{"aExcl"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]int
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 1, resp["restored"])
}

// TestExcludedAssetsHTTPSuccess 正常路径:只返回 origin=2 的排除清单。
func TestExcludedAssetsHTTPSuccess(t *testing.T) {
	e, db := newAssetsTestEcho(t)
	_, err := db.Exec(`INSERT INTO smart_views(id,name,conds_raw,conds_parsed,threshold,live) VALUES('sv-1','V','[]','[]',70,0)`)
	require.NoError(t, err)
	for _, id := range []string{"aExcl", "aAuto"} {
		_, err = db.Exec(`INSERT INTO assets(id,file_path,status) VALUES(?,?,'indexed')`, id, "/p/"+id+".jpg")
		require.NoError(t, err)
	}
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-1','aExcl',0.6,2)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO smart_view_matches(smart_view_id,asset_id,match_score,origin) VALUES('sv-1','aAuto',0.6,0)`)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/v1/photos/smart-views/sv-1/excluded", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var assets []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &assets))
	require.Len(t, assets, 1)
	require.Equal(t, "aExcl", assets[0]["id"])
}

// TestFromAlbumHTTPNotFound albumId 不存在应返回 404。
func TestFromAlbumHTTPNotFound(t *testing.T) {
	e := newTestEcho(t)
	body, _ := json.Marshal(map[string]any{"albumId": "missing"})
	req := httptest.NewRequest(http.MethodPost, "/v1/photos/smart-views/from-album", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestFromAlbumHTTPBadRequest 缺 albumId 应返回 400。
func TestFromAlbumHTTPBadRequest(t *testing.T) {
	e := newTestEcho(t)
	body, _ := json.Marshal(map[string]any{})
	req := httptest.NewRequest(http.MethodPost, "/v1/photos/smart-views/from-album", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestFromAlbumHTTPSuccess 正常路径:手动相册变身为智能相册,原相册消失,
// 响应带 createdAt。
func TestFromAlbumHTTPSuccess(t *testing.T) {
	e, db := newAssetsTestEcho(t)
	_, err := db.Exec(`INSERT INTO albums(id,name) VALUES('al-1','Trip')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('a1','/p/a1.jpg','indexed',0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO album_assets(album_id,asset_id) VALUES('al-1','a1')`)
	require.NoError(t, err)

	body, _ := json.Marshal(map[string]any{"albumId": "al-1"})
	req := httptest.NewRequest(http.MethodPost, "/v1/photos/smart-views/from-album", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"createdAt"`)

	var sv service.SmartView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sv))
	require.Equal(t, "Trip", sv.Name)
	require.True(t, sv.Live)

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM albums WHERE id='al-1'`).Scan(&n))
	require.Equal(t, 0, n, "原相册应已删除")
}

func TestSmartViewHTTPCreateAndList(t *testing.T) {
	e := newTestEcho(t)
	body, _ := json.Marshal(map[string]any{
		"id": "sv-h", "name": "HTTP", "condsRaw": []string{}, "threshold": 70, "live": true,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/photos/smart-views", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	req = httptest.NewRequest(http.MethodGet, "/v1/photos/smart-views", nil)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var list []service.SmartView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 1)
	require.Equal(t, "HTTP", list[0].Name)
}
