package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/stretchr/testify/require"
)

func TestHandleCreatedPathsEnqueuesFilesAndWalksDirs(t *testing.T) {
	dir := t.TempDir()
	album := filepath.Join(dir, "album")
	require.NoError(t, os.Mkdir(album, 0o755))
	inDir := filepath.Join(album, "b.png")
	single := filepath.Join(dir, "a.jpg")
	junk := filepath.Join(dir, "c.txt")
	for _, p := range []string{inDir, single, junk} {
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	}

	db, err := sqlite.Open(filepath.Join(t.TempDir(), "p.db"))
	require.NoError(t, err)
	defer db.Close()
	ix := NewIndexer(db, nil, t.TempDir(), 1)

	handleCreatedPaths(context.Background(), ix, []string{album, single, junk, filepath.Join(dir, "gone.jpg")})

	// Enqueue records each accepted path in ix.seen (sync.Map) and never removes
	// it on the success path (queue is buffered, workers aren't started here), so
	// seen is a reliable record of what got enqueued.
	var got []string
	ix.seen.Range(func(k, _ any) bool {
		got = append(got, k.(string))
		return true
	})
	// Directory recursively expanded (album/b.png), single media file (a.jpg)
	// enqueued; non-media (c.txt) and gone (gone.jpg) discarded
	require.ElementsMatch(t, []string{inDir, single}, got)
}
