package service

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
)

type mockTextMLSV struct{}

func (m *mockTextMLSV) CLIPTextEmbed(_ string) ([]float32, error) {
	v := make([]float32, 512)
	v[0] = 1.0
	return v, nil
}

func svTestService(t *testing.T) *SmartViewService {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "sv.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	search := NewSearchService(db, &mockTextMLSV{})
	return NewSmartViewService(db, search)
}

func TestSmartViewCRUD(t *testing.T) {
	s := svTestService(t)
	in := SmartViewInput{ID: "sv-1", Name: "Test", CondsRaw: []string{"scene: sunset"}, Threshold: 70, Live: true}
	sv, err := s.Create(in)
	require.NoError(t, err)
	require.Equal(t, "sv-1", sv.ID)
	require.Equal(t, []string{"scene: sunset"}, sv.Conds)

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)

	got, err := s.Get("sv-1")
	require.NoError(t, err)
	require.Equal(t, "Test", got.Name)

	_, err = s.Update("sv-1", SmartViewPatch{Name: ptr("Renamed")})
	require.NoError(t, err)
	got, _ = s.Get("sv-1")
	require.Equal(t, "Renamed", got.Name)

	require.NoError(t, s.Delete("sv-1"))
	_, err = s.Get("sv-1")
	require.ErrorIs(t, err, ErrNotFound)
}

func ptr[T any](v T) *T { return &v }

func TestEvaluateIntersectionAndScore(t *testing.T) {
	s := svTestService(t)
	db := s.db
	for _, id := range []string{"a1", "a2", "a3"} {
		_, err := db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video,file_size,taken_at)
			VALUES(?,?,?,0,?,?)`, id, "/p/"+id+".jpg", "indexed", 1000, "2026-01-01T00:00:00Z")
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO asset_clip_idx(asset_id) VALUES(?)`, id)
		require.NoError(t, err)
		var rowid int64
		require.NoError(t, db.QueryRow(`SELECT rowid FROM asset_clip_idx WHERE asset_id=?`, id).Scan(&rowid))
		vec := make([]float32, 512)
		vec[0] = 1.0
		_, err = db.Exec(`INSERT INTO clip_embeddings(rowid,embedding) VALUES(?,?)`, rowid, sqlite.SerializeFloat32(vec))
		require.NoError(t, err)
	}
	_, _ = db.Exec(`INSERT INTO persons(id,name) VALUES('p-sara','Sara')`)
	for _, fid := range []struct{ f, a string }{{"f1", "a1"}, {"f2", "a2"}} {
		_, _ = db.Exec(`INSERT INTO face_detections(id,asset_id,bbox,embedding) VALUES(?,?,'{}',X'00')`, fid.f, fid.a)
		_, _ = db.Exec(`INSERT INTO face_person(face_id,person_id) VALUES(?, 'p-sara')`, fid.f)
	}
	in := SmartViewInput{ID: "sv-x", Name: "Sara sunsets",
		CondsRaw: []string{"Sara", "scene: sunset"}, Threshold: 50, Live: true}
	_, err := s.Create(in)
	require.NoError(t, err)

	var ids []string
	rows, _ := db.Query(`SELECT asset_id FROM smart_view_matches WHERE smart_view_id='sv-x' ORDER BY asset_id`)
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	require.Equal(t, []string{"a1", "a2"}, ids)

	var cnt int
	db.QueryRow(`SELECT COUNT(*) FROM smart_view_matches WHERE smart_view_id='sv-x' AND match_score>0`).Scan(&cnt)
	require.Equal(t, 2, cnt)
}

func TestEvaluatePureStructuralScoreIsOne(t *testing.T) {
	s := svTestService(t)
	db := s.db
	_, _ = db.Exec(`INSERT INTO assets(id,file_path,status,is_live_photo_video) VALUES('a1','/p/a1.jpg','indexed',0)`)
	_, _ = db.Exec(`INSERT INTO persons(id,name) VALUES('p-sara','Sara')`)
	_, _ = db.Exec(`INSERT INTO face_detections(id,asset_id,bbox,embedding) VALUES('f1','a1','{}',X'00')`)
	_, _ = db.Exec(`INSERT INTO face_person(face_id,person_id) VALUES('f1','p-sara')`)
	_, err := s.Create(SmartViewInput{ID: "sv-p", Name: "Sara", CondsRaw: []string{"Sara"}, Threshold: 90, Live: true})
	require.NoError(t, err)
	var score float64
	require.NoError(t, db.QueryRow(`SELECT match_score FROM smart_view_matches WHERE smart_view_id='sv-p' AND asset_id='a1'`).Scan(&score))
	require.Equal(t, 1.0, score)
}
