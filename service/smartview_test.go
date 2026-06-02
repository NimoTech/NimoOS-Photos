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
