package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/require"
)

// writeFile writes content to path (creating parent dirs as needed). Used
// repeatedly inside require.Eventually retry loops below: re-writing the
// same file on every poll tick is what makes these tests robust against the
// unavoidable race between spawning the Watcher goroutine and its inotify
// watches actually being registered — a single unretried write performed
// before the watch exists produces no event at all, so the assertion would
// otherwise flake.
func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// newWatcherTestHarness wires a real Indexer (worker pool running) and a real
// Watcher over a temp DB, mirroring the construction pattern used throughout
// indexer_test.go (makeTestDB + mockML + NewIndexer, idx.Start in a
// goroutine).
func newWatcherTestHarness(t *testing.T, ctx context.Context, watchDirs []string) (*Watcher, *Indexer) {
	t.Helper()
	db := makeTestDB(t)
	idx := NewIndexer(db, &mockML{}, t.TempDir(), 2)
	go idx.Start(ctx)
	w := NewWatcher(db, watchDirs, idx, "")
	return w, idx
}

// assetIndexed reports whether path has an asset row with status='indexed'.
func assetIndexed(t *testing.T, idx *Indexer, path string) bool {
	t.Helper()
	var status string
	err := idx.db.QueryRow(`SELECT status FROM assets WHERE file_path=?`, path).Scan(&status)
	return err == nil && status == "indexed"
}

// TestWatcherRecursiveNestedSubdir verifies the core bug fix: a file written
// into a subdirectory several levels below a WatchDir root — never watched
// directly, only reachable via recursive Add at Start — is detected.
func TestWatcherRecursiveNestedSubdir(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	// Pre-existing nested file, present before Start ever runs.
	writeFile(t, filepath.Join(sub, "a.jpg"), "pre-existing")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, idx := newWatcherTestHarness(t, ctx, []string{root})
	go w.Start(ctx)

	newFile := filepath.Join(sub, "b.jpg")
	require.Eventually(t, func() bool {
		writeFile(t, newFile, "new")
		return assetIndexed(t, idx, newFile)
	}, 5*time.Second, 100*time.Millisecond,
		"file written into a pre-existing nested subdirectory must be indexed")
}

// TestWatcherDynamicNewDirectory covers both windows a plain recursive Add at
// startup cannot: (a) mkdir now, drop a file in shortly after, and (b) an
// entire directory — with a file already inside it — moved in as one atomic
// rename.
func TestWatcherDynamicNewDirectory(t *testing.T) {
	root := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, idx := newWatcherTestHarness(t, ctx, []string{root})
	go w.Start(ctx)

	// (a) mkdir after Start, then a file lands inside it.
	newDir := filepath.Join(root, "newsub")
	require.NoError(t, os.Mkdir(newDir, 0o755))
	newFile := filepath.Join(newDir, "c.jpg")
	require.Eventually(t, func() bool {
		writeFile(t, newFile, "new")
		return assetIndexed(t, idx, newFile)
	}, 5*time.Second, 100*time.Millisecond,
		"file written into a directory created after Start must be indexed")

	// (b) an entire directory, file already inside it, moved in atomically.
	// This must be picked up by the catch-up scan (trackNewDir), not by a
	// live Write/Create event on the file itself, since the file was never
	// created while anything was watching its original location.
	srcDir := filepath.Join(t.TempDir(), "movedin")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	movedFile := filepath.Join(srcDir, "e.jpg")
	writeFile(t, movedFile, "already-here")
	destDir := filepath.Join(root, "movedin")
	require.NoError(t, os.Rename(srcDir, destDir))

	destFile := filepath.Join(destDir, "e.jpg")
	require.Eventually(t, func() bool {
		return assetIndexed(t, idx, destFile)
	}, 5*time.Second, 100*time.Millisecond,
		"a directory moved in with a file already inside it must be caught up by the scan")
}

// TestWatcherSkipsHiddenDirectories verifies a hidden directory nested under
// a WatchDir is never watched, so files inside it (whether present before
// Start or added afterward) are never indexed by the watcher.
func TestWatcherSkipsHiddenDirectories(t *testing.T) {
	root := t.TempDir()
	hiddenDir := filepath.Join(root, ".hidden")
	require.NoError(t, os.MkdirAll(hiddenDir, 0o755))
	preFile := filepath.Join(hiddenDir, "f.jpg")
	writeFile(t, preFile, "pre-existing")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, idx := newWatcherTestHarness(t, ctx, []string{root})
	go w.Start(ctx)

	// Prove the watcher is actually alive and processing events for the
	// (non-hidden) root, so a later "nothing happened" observation for
	// .hidden is meaningful rather than the watcher simply not having
	// started yet.
	warmFile := filepath.Join(root, "warmup.jpg")
	require.Eventually(t, func() bool {
		writeFile(t, warmFile, "warm")
		return assetIndexed(t, idx, warmFile)
	}, 5*time.Second, 100*time.Millisecond, "warmup file must be indexed")

	postFile := filepath.Join(hiddenDir, "g.jpg")
	writeFile(t, postFile, "post")

	require.Never(t, func() bool {
		return assetIndexed(t, idx, preFile) || assetIndexed(t, idx, postFile)
	}, 1500*time.Millisecond, 100*time.Millisecond,
		"files inside a hidden directory must never be indexed by the watcher")
}

// TestWatcherRuntimeHiddenDirNotTracked is the regression test for the
// root-exemption leak in trackNewDir: a hidden directory created at runtime
// (the exact shape TrashService produces — .trash/<id>/ under a WatchDir)
// must NOT be recursively watched, and media files landing inside it must
// never be enqueued. Without the trackNewDir entry guard, the dynamically
// discovered directory was passed to addRecursiveWatch as "root" and
// inherited the hidden-check exemption that is meant only for explicitly
// configured WatchDirs — every soft-delete would then leak an inotify watch
// and re-index the trashed file, violating the "soft-deleted files are never
// re-indexed" invariant (see walkSupported in indexer.go).
func TestWatcherRuntimeHiddenDirNotTracked(t *testing.T) {
	root := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, idx := newWatcherTestHarness(t, ctx, []string{root})
	go w.Start(ctx)

	// Warmup: prove the watcher is alive before asserting a negative.
	warmFile := filepath.Join(root, "warmup.jpg")
	require.Eventually(t, func() bool {
		writeFile(t, warmFile, "warm")
		return assetIndexed(t, idx, warmFile)
	}, 5*time.Second, 100*time.Millisecond, "warmup file must be indexed")

	// Runtime mkdir of a hidden directory, then a media file inside it —
	// mirrors TrashService creating .trash/<id>/ on first soft-delete.
	trashDir := filepath.Join(root, ".trash", "id1")
	require.NoError(t, os.MkdirAll(trashDir, 0o755))
	trashedFile := filepath.Join(trashDir, "deleted.jpg")
	writeFile(t, trashedFile, "soft-deleted")

	require.Never(t, func() bool {
		return assetIndexed(t, idx, trashedFile)
	}, 1500*time.Millisecond, 100*time.Millisecond,
		"a file inside a runtime-created hidden directory must never be indexed")
}

// TestWatcherWatchDirSymlinkRoot verifies a WatchDir that is itself a symlink
// to a real directory still produces watches. The old non-recursive fw.Add
// followed the symlink (inotify_add_watch resolves symlinks); a naive
// filepath.WalkDir lstat's the root and would silently yield zero watches —
// a behaviour regression this test pins down.
func TestWatcherWatchDirSymlinkRoot(t *testing.T) {
	// Resolve the temp dir itself first so the expected event path (what the
	// watcher reports after resolving the symlinked root) matches exactly.
	realDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	linkDir := filepath.Join(t.TempDir(), "gallery-link")
	require.NoError(t, os.Symlink(realDir, linkDir))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, idx := newWatcherTestHarness(t, ctx, []string{linkDir})
	go w.Start(ctx)

	// Events carry the resolved (real) path since that is what gets watched.
	newFile := filepath.Join(realDir, "via-symlink.jpg")
	require.Eventually(t, func() bool {
		writeFile(t, newFile, "new")
		return assetIndexed(t, idx, newFile)
	}, 5*time.Second, 100*time.Millisecond,
		"a WatchDir configured as a symlink must still be watched (resolved)")
}

// TestWatcherRuntimeSymlinkDirNotTracked is the regression test for the
// symlink-resolution leak (same shape as the hidden-dir root-exemption leak):
// EvalSymlinks living inside the shared addRecursiveWatch meant a symlink to
// a directory created at runtime inside a watched tree got resolved and its
// ENTIRE external target tree recursively watched and indexed — a symlink
// pointing at / would watch nearly the whole filesystem, blow the inotify
// quota, and pull out-of-library content into the DB, diverging from the
// periodic scan (walkSupported never follows symlinks). Only explicitly
// configured WatchDir roots may be resolved (in Start); dynamically
// discovered directories keep WalkDir's lstat semantics and are simply not
// tracked when they are symlinks.
func TestWatcherRuntimeSymlinkDirNotTracked(t *testing.T) {
	root := t.TempDir()
	external, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, idx := newWatcherTestHarness(t, ctx, []string{root})
	go w.Start(ctx)

	// Warmup: prove the watcher is alive before asserting a negative.
	warmFile := filepath.Join(root, "warmup.jpg")
	require.Eventually(t, func() bool {
		writeFile(t, warmFile, "warm")
		return assetIndexed(t, idx, warmFile)
	}, 5*time.Second, 100*time.Millisecond, "warmup file must be indexed")

	// Runtime symlink inside the watched tree, pointing at an external dir.
	linkDir := filepath.Join(root, "linkdir")
	require.NoError(t, os.Symlink(external, linkDir))

	externalFile := filepath.Join(external, "outside.jpg")
	linkFile := filepath.Join(linkDir, "outside.jpg")
	require.Never(t, func() bool {
		writeFile(t, externalFile, "outside")
		return assetIndexed(t, idx, externalFile) || assetIndexed(t, idx, linkFile)
	}, 1500*time.Millisecond, 100*time.Millisecond,
		"a runtime symlink to an external directory must not pull its tree into the index")
}

// TestWatcherRestartSwitchesWatchDirs verifies the hot-reload path: after
// Restart, the new WatchDirs take effect and the old ones stop triggering.
func TestWatcherRestartSwitchesWatchDirs(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, idx := newWatcherTestHarness(t, ctx, []string{oldDir})
	go w.Start(ctx)

	// Prove oldDir is really watched before restarting away from it.
	beforeFile := filepath.Join(oldDir, "before.jpg")
	require.Eventually(t, func() bool {
		writeFile(t, beforeFile, "before")
		return assetIndexed(t, idx, beforeFile)
	}, 5*time.Second, 100*time.Millisecond, "oldDir must be watched before Restart")

	w.Restart(ctx, []string{newDir})

	// newDir must now be watched.
	afterFile := filepath.Join(newDir, "after.jpg")
	require.Eventually(t, func() bool {
		writeFile(t, afterFile, "after")
		return assetIndexed(t, idx, afterFile)
	}, 5*time.Second, 100*time.Millisecond, "newDir must be watched after Restart")

	// oldDir must no longer be watched.
	oldFile2 := filepath.Join(oldDir, "old2.jpg")
	require.Never(t, func() bool {
		writeFile(t, oldFile2, "old2")
		return assetIndexed(t, idx, oldFile2)
	}, 1500*time.Millisecond, 100*time.Millisecond,
		"oldDir must not be watched after Restart")
}

// TestHandleWatchErrorOverflowTriggersRescan verifies fsnotify.ErrEventOverflow
// (the inotify event queue overflowing, which silently drops events) triggers
// an async recovery rescan of all watch roots so files whose events were lost
// still get indexed eventually. A non-overflow error must NOT trigger a
// rescan (and must not panic).
func TestHandleWatchErrorOverflowTriggersRescan(t *testing.T) {
	root := t.TempDir()
	preFile := filepath.Join(root, "a.jpg")
	writeFile(t, preFile, "pre-existing")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, idx := newWatcherTestHarness(t, ctx, []string{root})
	w.roots = []string{root}

	// Non-overflow error: must not panic and must not trigger a rescan.
	w.handleWatchError(context.Background(), errors.New("boom"))
	require.Never(t, func() bool {
		return assetIndexed(t, idx, preFile)
	}, 500*time.Millisecond, 50*time.Millisecond,
		"a non-overflow watch error must not trigger a rescan")

	w.handleWatchError(context.Background(), fsnotify.ErrEventOverflow)
	require.Eventually(t, func() bool {
		return assetIndexed(t, idx, preFile)
	}, 5*time.Second, 100*time.Millisecond,
		"queue overflow must trigger a rescan that (re-)enqueues existing files")
}

func TestWalkSupportedHonorsCtxCancel(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.jpg", i)), []byte("x"), 0o644))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 进入前已取消:一个文件都不应回调
	calls := 0
	err := walkSupported(ctx, dir, func(string) { calls++ })
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 0, calls)
}
