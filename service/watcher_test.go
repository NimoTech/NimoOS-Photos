package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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

// seedPhotoFile writes a pre-existing "photo" at path before the watcher/
// indexer ever start — the same minimal-content recipe writeFile uses
// elsewhere in this file (assetIndexed only checks the assets row's status,
// not real image bytes). Named separately from writeFile purely for
// readability at call sites where the point is "this file already exists
// before Start runs" (only a catch-up scan, not a live fsnotify event, can
// discover it).
func seedPhotoFile(t *testing.T, path string) {
	t.Helper()
	writeFile(t, path, "seed")
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

// assetExists reports whether path still has any asset row at all, regardless
// of status.
func assetExists(t *testing.T, idx *Indexer, path string) bool {
	t.Helper()
	var n int
	err := idx.db.QueryRow(`SELECT COUNT(*) FROM assets WHERE file_path=?`, path).Scan(&n)
	return err == nil && n > 0
}

// TestWatcherHandleEventPrunesDirectoryDelete covers the bug where deleting
// an entire subdirectory left every asset indexed from it (CLIP vector and
// thumbnail included) permanently stuck in the DB until the next 24h
// ScanAllRoots: fsnotify's Remove/Rename event for a directory carries the
// directory's own path as event.Name, which has no recognised media
// extension. Before the fix, handleEvent's isSupportedMedia-only branch
// silently dropped such events.
//
// This deliberately drives handleEvent directly with a synthetic event
// (mirroring TestHandleWatchErrorOverflowTriggersRescan's approach below)
// rather than deleting real files via a live Watcher + os.RemoveAll: on a
// plain local filesystem, RemoveAll unlinks every file individually, and
// each unlink already produces its own per-file Remove event that the
// PRE-EXISTING isSupportedMedia+RemoveByPath branch handles correctly —
// making an end-to-end RemoveAll test pass regardless of whether this fix
// exists, and thus unable to actually prove it. The dropped-event gap this
// fix closes is about event.Name itself being a bare directory path (e.g. a
// directory renamed out of the watched tree, or a coarse-grained notification
// from a network/FUSE watch dir such as NimoOS's rclone cloud mounts) — this
// test targets exactly that dispatch path, deterministically.
func TestWatcherHandleEventPrunesDirectoryDelete(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub") // already gone from disk by the time the event is handled

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db := makeTestDB(t)
	idx := NewIndexer(db, &mockML{}, t.TempDir(), 2)
	// t.TempDir() is not a real mount point, so pruneMissingUnder's
	// dirUnderMountedRoot guard would otherwise refuse to touch it — override
	// mountRoots the same way indexer_test.go's prune tests do.
	idx.mountRoots = func() []string { return []string{root} }
	go idx.Start(ctx)

	fileA := filepath.Join(sub, "a.jpg")
	fileB := filepath.Join(sub, "b.jpg")
	insertAsset(t, db, fileA, "indexed")
	insertAsset(t, db, fileB, "indexed")

	w := NewWatcher(db, []string{root}, idx, "")
	fw, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	defer fw.Close()
	var wg sync.WaitGroup
	defer wg.Wait()

	w.handleEvent(ctx, fw, &wg, fsnotify.Event{Name: sub, Op: fsnotify.Remove})

	require.Eventually(t, func() bool {
		return !assetExists(t, idx, fileA) && !assetExists(t, idx, fileB)
	}, 5*time.Second, 100*time.Millisecond,
		"目录删除事件(event.Name 为无扩展名的目录路径)必须清理该目录下所有 asset,而不是被 isSupportedMedia 白名单静默丢弃")
}

// TestWatcherRemovesAssetOnFileDelete is a regression guard for the existing
// single-file delete path: a Remove/Rename event whose event.Name carries a
// supported media extension must keep going through the exact-match
// RemoveByPath fast path, unaffected by the directory-delete fix above.
func TestWatcherRemovesAssetOnFileDelete(t *testing.T) {
	root := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w, idx := newWatcherTestHarness(t, ctx, []string{root})
	go w.Start(ctx)

	file := filepath.Join(root, "solo.jpg")
	require.Eventually(t, func() bool {
		writeFile(t, file, "data")
		return assetIndexed(t, idx, file)
	}, 5*time.Second, 100*time.Millisecond, "文件应先被索引")

	require.NoError(t, os.Remove(file))

	require.Eventually(t, func() bool {
		return !assetExists(t, idx, file)
	}, 5*time.Second, 100*time.Millisecond, "删除单个文件后,对应 asset 记录应被清理")
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

// TestWatcherAutoModeWatchesEnumeratedRoots:WatchDirs 为空 ⇒ 根集合来自
// enumerateRoots(生产=EnumerateScanRoots)。往枚举出的根里丢照片必须被
// 实时 Enqueue 索引——这是"NAS 全空间实时监控"的核心行为。
func TestWatcherAutoModeWatchesEnumeratedRoots(t *testing.T) {
	db := makeTestDB(t)
	root := t.TempDir()
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	w := NewWatcher(db, nil, ix, "") // 空 watchDirs = 自动模式
	w.enumerateRoots = func() []string { return []string{root} }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ix.Start(ctx)
	go w.Start(ctx)

	newFile := filepath.Join(root, "photo.jpg")
	require.Eventually(t, func() bool {
		writeFile(t, newFile, "new")
		return assetIndexed(t, ix, newFile)
	}, 5*time.Second, 100*time.Millisecond,
		"auto 模式下,enumerateRoots 枚举出的根必须被实时监控")
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
	var wg sync.WaitGroup
	defer wg.Wait()

	// Non-overflow error: must not panic and must not trigger a rescan.
	w.handleWatchError(context.Background(), &wg, errors.New("boom"))
	require.Never(t, func() bool {
		return assetIndexed(t, idx, preFile)
	}, 500*time.Millisecond, 50*time.Millisecond,
		"a non-overflow watch error must not trigger a rescan")

	w.handleWatchError(context.Background(), &wg, fsnotify.ErrEventOverflow)
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

func TestWalkSupportedSkipsUnreadableEntry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "a-locked")
	require.NoError(t, os.Mkdir(bad, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "z-ok.jpg"), []byte("x"), 0o644))
	require.NoError(t, os.Chmod(bad, 0o000))
	t.Cleanup(func() { _ = os.Chmod(bad, 0o755) })

	var got []string
	err := walkSupported(context.Background(), dir, func(p string) { got = append(got, p) })
	require.NoError(t, err) // 坏子目录被跳过,不再中止整树
	require.Equal(t, []string{filepath.Join(dir, "z-ok.jpg")}, got)
}

// TestSkipCatchupScan pins down skipCatchupScan's semantics: added==0 alone
// must NOT mean "skip the catch-up scan" — it is ambiguous between "directory
// excluded/vanished" (safe to skip: no watch coverage was ever intended) and
// "every fw.Add failed on ENOSPC" (files still exist and must be indexed;
// only future-change tracking is degraded until the inotify quota is raised).
func TestSkipCatchupScan(t *testing.T) {
	require.True(t, skipCatchupScan(0, false))  // 目录被排除/已消失:跳过
	require.False(t, skipCatchupScan(0, true))  // 全因 ENOSPC 失败:不得跳过
	require.False(t, skipCatchupScan(3, false)) // 正常:不跳过
	require.False(t, skipCatchupScan(3, true))  // 部分 ENOSPC:不跳过
}

// TestTrackNewDirDedup pins down walkCovered's semantics: a recursive walk
// already in flight for an ancestor (or the directory itself) must be treated
// as covering it, so a dense mkdir burst under that ancestor doesn't spawn
// redundant overlapping walks.
func TestTrackNewDirDedup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, _ := newWatcherTestHarness(t, ctx, nil) // reuse existing harness; nil watchDirs is fine
	root := t.TempDir()
	child := filepath.Join(root, "a", "b")
	require.NoError(t, os.MkdirAll(child, 0o755))

	// 手动占位:root 的 walk 在途
	w.walkInFlight.Store(withSep(root), struct{}{})
	require.True(t, w.walkCovered(child))        // 祖先在途 → 覆盖
	require.True(t, w.walkCovered(root))         // 自身在途 → 覆盖
	require.False(t, w.walkCovered(t.TempDir())) // 无关目录 → 不覆盖
}

// TestTrackNewDirWatchesDespiteAncestorCoverage is the regression test for
// the Task 4 dedup bug: walkCovered must gate ONLY the redundant catch-up
// index scan, never addRecursiveWatch itself. filepath.WalkDir is a single
// pre-order pass — a subdirectory created under an ancestor AFTER that
// ancestor's walk already snapshotted its children is never revisited by the
// ancestor's walk, so if trackNewDir also skips watching such a directory
// whenever walkCovered reports true, that directory is left permanently
// unwatched: files dropped into it afterward go undetected until the next
// 24h full rescan.
//
// The test simulates exactly that: it manually pre-stores an ANCESTOR's key
// in walkInFlight (so walkCovered(child) is true, mirroring an ancestor scan
// still "in flight"), then calls trackNewDir directly on a real child
// directory that already contains a media file. It asserts two things:
//   - the pre-existing file is NOT enqueued by trackNewDir's own catch-up
//     scan (dedup still applies to the expensive indexing walk — the fake
//     ancestor "covers" it), and
//   - a file written into that same child directory AFTER trackNewDir
//     returns DOES get indexed via a live fsnotify event, proving
//     addRecursiveWatch actually ran and registered a real inotify watch on
//     child despite walkCovered==true.
//
// Against the pre-fix code (which returns out of trackNewDir immediately
// when walkCovered is true, before ever calling addRecursiveWatch), the
// second assertion fails: child is never watched, so the post-return write
// produces no event and the file is never indexed.
func TestTrackNewDirWatchesDespiteAncestorCoverage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root := t.TempDir()
	child := filepath.Join(root, "child")
	require.NoError(t, os.MkdirAll(child, 0o755))
	existingFile := filepath.Join(child, "existing.jpg")
	writeFile(t, existingFile, "pre-existing")

	w, idx := newWatcherTestHarness(t, ctx, nil)

	fw, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	defer fw.Close()

	var wg sync.WaitGroup
	defer wg.Wait()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-fw.Events:
				if !ok {
					return
				}
				w.handleEvent(ctx, fw, &wg, event)
			case _, ok := <-fw.Errors:
				if !ok {
					return
				}
			}
		}
	}()

	// Simulate an ancestor's recursive walk already in flight, covering child.
	w.walkInFlight.Store(withSep(root), struct{}{})

	w.trackNewDir(ctx, fw, child)

	// Dedup must still suppress the redundant catch-up scan: the pre-existing
	// file must not have been enqueued by trackNewDir itself.
	require.Never(t, func() bool {
		return assetIndexed(t, idx, existingFile)
	}, 500*time.Millisecond, 50*time.Millisecond,
		"catch-up scan must stay deduped when an ancestor walk is in flight")

	// But watch coverage must NOT have been dropped: a file written into
	// child after trackNewDir returns must still be picked up live.
	newFile := filepath.Join(child, "new.jpg")
	require.Eventually(t, func() bool {
		writeFile(t, newFile, "new")
		return assetIndexed(t, idx, newFile)
	}, 5*time.Second, 100*time.Millisecond,
		"child directory must still be watched even though an ancestor scan is in flight (walkCovered must gate only the catch-up scan, not addRecursiveWatch)")
}

// TestWatcherFollowsMountChanges:自动模式下根集合快照变化(新盘挂上)必须
// 在一个轮询周期内触发重启纳入监控,且对新根做一次补扫让存量文件入库。
func TestWatcherFollowsMountChanges(t *testing.T) {
	db := makeTestDB(t)
	rootA, rootB := t.TempDir(), t.TempDir()
	// rootB 里预置一张"存量"照片:只有补扫才会发现它(inotify 只看未来事件)。
	seedPhotoFile(t, filepath.Join(rootB, "existing.jpg"))

	var phase atomic.Int32
	ix := NewIndexer(db, &mockML{}, t.TempDir(), 1)
	w := NewWatcher(db, nil, ix, "")
	w.pollInterval = 20 * time.Millisecond
	w.enumerateRoots = func() []string {
		if phase.Load() == 0 {
			return []string{rootA}
		}
		return []string{rootA, rootB}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 启动方式照抄 TestWatcherAutoModeWatchesEnumeratedRoots。
	go ix.Start(ctx)
	go w.Start(ctx)

	// warmup:先证明 watcher 已经用 phase=0 的快照跑起来(rootA 已被实时监
	// 控),排除"phase.Store(1) 抢在 Start 首次 enumerateRoots() 之前执行"
	// 的时序竞争——手法与 TestWatcherRestartSwitchesWatchDirs 的 warmup 一致。
	warmFile := filepath.Join(rootA, "warmup.jpg")
	require.Eventually(t, func() bool {
		writeFile(t, warmFile, "warm")
		return assetIndexed(t, ix, warmFile)
	}, 5*time.Second, 100*time.Millisecond, "rootA 的 warmup 文件必须先被索引,证明 watcher 已用初始快照跑起来")

	// 模拟新盘挂载:下一轮询周期起,enumerateRoots 会枚举出 rootB。
	phase.Store(1)

	// 断言 1:existing.jpg 在超时内入库(补扫生效)。
	existingFile := filepath.Join(rootB, "existing.jpg")
	require.Eventually(t, func() bool {
		return assetIndexed(t, ix, existingFile)
	}, 5*time.Second, 100*time.Millisecond,
		"新挂载的根里预置的存量文件必须被补扫入库")

	// 断言 2:再往 rootB 丢 new.jpg 也在超时内入库(重启后已在监控)。
	newFile := filepath.Join(rootB, "new.jpg")
	require.Eventually(t, func() bool {
		writeFile(t, newFile, "new")
		return assetIndexed(t, ix, newFile)
	}, 5*time.Second, 100*time.Millisecond,
		"新挂载的根必须在重启后被实时监控")
}

// TestDiffNewRoots:集合差集,顺序无关。
func TestDiffNewRoots(t *testing.T) {
	require.Equal(t, []string{"/b"}, diffNewRoots([]string{"/a"}, []string{"/b", "/a"}))
	require.Empty(t, diffNewRoots([]string{"/a", "/b"}, []string{"/b", "/a"}))
}
