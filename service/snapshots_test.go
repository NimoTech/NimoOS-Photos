package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIsInSnapshotsDir covers the shared predicate's core contract: a match
// only counts if it's a complete path segment; a plain substring match (e.g.
// "my.snapshots.backup") must never be misjudged as a hit.
func TestIsInSnapshotsDir(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/media/RAID_0/.snapshots/20260716T200714Z_auto-hourly/Image/a.jpg", true},
		{"/media/RAID_0/.snapshots", true},
		{"/media/RAID_0/.snapshots/", true},
		{"/.snapshots/x", true},
		// Multi-level nesting must also hit: the root itself is deeper inside .snapshots.
		{"/media/RAID_0/.snapshots/ts/sub/deeper/x.jpg", true},
		// Only a partial/substring match, not an independent path segment, must never be misjudged.
		{"/media/RAID_0/my.snapshots.backup/a.jpg", false},
		{"/media/RAID_0/.snapshotsfoo/a.jpg", false},
		{"/media/RAID_0/foo.snapshots/a.jpg", false},
		{"/media/RAID_0/normal/a.jpg", false},
		{"/DATA/Gallery/photo.jpg", false},
	}
	for _, c := range cases {
		require.Equal(t, c.want, isInSnapshotsDir(c.path), "isInSnapshotsDir(%q)", c.path)
	}
}

// TestWalkSupportedSkipsNestedSnapshotsDir: when normally scanning a volume's
// root directory, encountering a subdirectory named .snapshots (no matter how
// deeply nested) must SkipDir it entirely, without affecting sibling/parent
// normal files.
func TestWalkSupportedSkipsNestedSnapshotsDir(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "keep.jpg")
	require.NoError(t, os.WriteFile(keep, []byte("x"), 0o644))

	// Simulates the real snapshot directory shape
	// <volume mount>/.snapshots/<ts>/Image/xxx.jpg, nested two levels to prove
	// it's not just blocking the .snapshots level itself.
	snapDeep := filepath.Join(root, ".snapshots", "20260716T200714Z_auto-hourly", "Image")
	require.NoError(t, os.MkdirAll(snapDeep, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(snapDeep, "a.jpg"), []byte("y"), 0o644))

	var collected []string
	require.NoError(t, walkSupported(context.Background(), root, func(p string) {
		collected = append(collected, p)
	}))

	require.Equal(t, []string{keep}, collected,
		"files under the .snapshots directory tree must be skipped entirely; only normal files should be collected")
}

// TestWalkSupportedSkipsWhenRootItselfIsInsideSnapshots covers the root cause
// of a real incident: btrbk/snapper mount each snapshot subvolume separately,
// so the walk's own root (not a subdirectory encountered during the walk) is
// already inside .snapshots — in this case "skip during traversal" never
// fires (the root node is never evaluated as its own subdirectory), so it
// must be intercepted separately at the entry point.
func TestWalkSupportedSkipsWhenRootItselfIsInsideSnapshots(t *testing.T) {
	root := t.TempDir()
	snapRoot := filepath.Join(root, ".snapshots", "20260716T200714Z_auto-hourly")
	require.NoError(t, os.MkdirAll(filepath.Join(snapRoot, "Image"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(snapRoot, "Image", "a.jpg"), []byte("y"), 0o644))

	var collected []string
	require.NoError(t, walkSupported(context.Background(), snapRoot, func(p string) {
		collected = append(collected, p)
	}))

	require.Empty(t, collected, "when the walk's own root is under .snapshots, it must be skipped entirely with no files collected")
}

// TestPruneSnapshotAssets covers startup cleanup: existing contaminated rows
// (path containing /.snapshots/) must go through the full asset deletion
// flow and be cleaned up completely (including CLIP vectors and face rows),
// while normal paths and similar paths that are only a "partial match"
// (my.snapshots.backup) must be preserved untouched.
func TestPruneSnapshotAssets(t *testing.T) {
	db := makeTestDB(t)

	contaminated := insertAsset(t, db,
		"/media/RAID_0/.snapshots/20260716T200714Z_auto-hourly/Image/a.jpg", "indexed")
	seedFaceAndClip(t, db, contaminated)

	keep := insertAsset(t, db, "/media/RAID_0/Image/a.jpg", "indexed")
	seedFaceAndClip(t, db, keep)

	// Only the path substring "looks like" a snapshot directory; it's actually
	// an ordinary user directory and must never be mistakenly deleted.
	lookalike := insertAsset(t, db, "/media/RAID_0/my.snapshots.backup/a.jpg", "indexed")
	seedFaceAndClip(t, db, lookalike)

	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.pruneSnapshotAssets()

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, contaminated).Scan(&n))
	require.Equal(t, 0, n, "contaminated assets under a snapshot directory must be hard-deleted")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM asset_clip_idx WHERE asset_id=?`, contaminated).Scan(&n))
	require.Equal(t, 0, n, "the contaminated asset's CLIP mapping must be cleaned up")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM face_detections WHERE asset_id=?`, contaminated).Scan(&n))
	require.Equal(t, 0, n, "the contaminated asset's face rows must be cleaned up")

	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, keep).Scan(&n))
	require.Equal(t, 1, n, "the normal asset must be unaffected")
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, lookalike).Scan(&n))
	require.Equal(t, 1, n, "an asset with only a similar path substring (my.snapshots.backup) must not be mistakenly deleted")
}

// TestPruneSnapshotAssetsNoContamination covers the zero-hit scenario: having
// no contaminated rows must not cause an error or mistakenly delete anything.
func TestPruneSnapshotAssetsNoContamination(t *testing.T) {
	db := makeTestDB(t)
	keep := insertAsset(t, db, "/DATA/Gallery/a.jpg", "indexed")

	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	ix.pruneSnapshotAssets()

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM assets WHERE id=?`, keep).Scan(&n))
	require.Equal(t, 1, n)
}
