package service

import (
	"bytes"
	"context"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestBackfillGuard_RealCorruptSamples replays the machine-138 wedge (2026-07):
// a library containing 0-byte mp4/png, all-0x00 recovery artifacts named
// .jpg, macOS AppleDouble ._x.jpg stubs, and m4v with N/A duration kept the
// disk at full sequential read for a day with zero progress, because every
// backfill round re-selected and re-read every permanently failing file.
//
// The invariant: after two back-to-back Backfill rounds the candidate set
// MUST be empty — each sample either has an embedding (and no ledger row)
// or has been recorded exactly once and is cooling down.
//
// Samples are real user data, never committed. Run with:
//
//	PHOTOS_CORRUPT_SAMPLES=/DATA/Downloads/photos-corrupt-samples-20260730 \
//	  CGO_ENABLED=1 go test ./service/ -run TestBackfillGuard_RealCorruptSamples -v
func TestBackfillGuard_RealCorruptSamples(t *testing.T) {
	dir := os.Getenv("PHOTOS_CORRUPT_SAMPLES")
	if dir == "" {
		t.Skip("PHOTOS_CORRUPT_SAMPLES not set")
	}
	for name, ml := range map[string]MLProvider{
		"permissive-ml": &mockML{},
		"decoding-ml":   &decodingML{},
	} {
		t.Run(name, func(t *testing.T) {
			db := makeTestDB(t)
			tmp := t.TempDir()
			var paths []string
			require.NoError(t, filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				ext := strings.ToLower(filepath.Ext(p))
				if _, ok := supportedExts[ext]; !ok {
					return nil
				}
				insertAsset(t, db, p, "indexed")
				paths = append(paths, p)
				return nil
			}))
			require.NotEmpty(t, paths, "sample dir contains no supported media")

			ix := NewIndexer(db, ml, filepath.Join(tmp, "thumbs"), 1)
			e := NewEmbedder(db, ml, ix, NewTaskRegistry(nil))
			require.NoError(t, e.Backfill(context.Background()))
			require.NoError(t, e.Backfill(context.Background()))

			leftover, err := e.queryMissing(context.Background(), time.Now())
			require.NoError(t, err)
			require.Empty(t, leftover,
				"candidate set must converge after two rounds, or the next recovery chain re-reads everything again")

			for _, p := range paths {
				if e.hasEmbeddingForPath(p) {
					continue
				}
				var id string
				require.NoError(t, db.QueryRow(`SELECT id FROM assets WHERE file_path=?`, p).Scan(&id))
				n, _ := readBackfillFailure(t, db, backfillCLIP, id)
				require.Equal(t, 1, n, "%s: recorded once, not once per pass", p)
			}
		})
	}
}

var _ = image.Decode // keep imports honest if decodingML moves
var _ = bytes.NewReader
