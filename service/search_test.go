package service_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
	"github.com/stretchr/testify/require"
)

type mockTextML struct{}

func (m *mockTextML) CLIPTextEmbed(_ string) ([]float32, error) {
	v := make([]float32, 512)
	v[0] = 1.0
	return v, nil
}

func openSearchDB(t *testing.T) *service.SearchService {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return service.NewSearchService(db, &mockTextML{})
}

func TestSmartSearch(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "s.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id, file_path, status, is_live_photo_video) VALUES('a1','/p/beach.jpg','indexed',0)`)
	db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES('a1')`)
	var rowid int64
	db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id='a1'`).Scan(&rowid)

	vec := make([]float32, 512)
	vec[0] = 1.0
	db.Exec(`INSERT INTO clip_embeddings(rowid, embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))

	svc := service.NewSearchService(db, &mockTextML{})
	results, err := svc.SmartSearch("beach", 10, service.SearchFilters{})
	require.NoError(t, err)
	require.NotEmpty(t, results)
	require.Equal(t, "a1", results[0].ID)
}

func TestTimeline(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "tl.db"))
	require.NoError(t, err)
	defer db.Close()

	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('a1','/p1.jpg','indexed','2025-07-15 10:00:00',0)`)
	db.Exec(`INSERT INTO assets(id,file_path,status,taken_at,is_live_photo_video) VALUES('v1','/p1.mov','indexed','2025-07-15 10:00:00',1)`)

	svc := service.NewSearchService(db, nil)
	groups, err := svc.Timeline()
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, 2025, groups[0].Year)
	require.Equal(t, 7, groups[0].Month)
	require.Len(t, groups[0].Assets, 1, "live photo video must be hidden")
}

func TestListAssets(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "la.db"))
	require.NoError(t, err)
	defer db.Close()

	for i := 0; i < 3; i++ {
		db.Exec(fmt.Sprintf(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('id%d','/p%d.jpg','indexed',0)`, i, i))
	}

	svc := service.NewSearchService(db, nil)
	assets, err := svc.ListAssets(10, 0)
	require.NoError(t, err)
	require.Len(t, assets, 3)
}
