package service

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/config"
	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	dbscanEpsilon    = 0.6
	dbscanMinPoints  = 1
	clusterBatchSize = 50
	// assignEpsilon is the max cosine distance for snapping a free-floating face
	// onto an anchored person's centroid.
	assignEpsilon = 0.55
	// suggestEpsilon is the cosine distance upper bound for "merge suggestion"
	// pairs (the lower bound is clusterEpsilon()).
	suggestEpsilon = 0.75
)

// clusterEpsilon returns the configured DBSCAN epsilon. Falls back to the
// legacy constant when config isn't initialized (tests constructing services
// directly keep their historical 0.6 semantics) or the value is non-positive.
func clusterEpsilon() float64 {
	if config.Cfg != nil && config.Cfg.ClusterEpsilon > 0 {
		return config.Cfg.ClusterEpsilon
	}
	return dbscanEpsilon
}

// clusterEngine returns the configured face-clustering engine selector.
// Falls back to "apple" when config isn't initialized or the value is empty
// (tests constructing services directly, or a config file predating this key).
func clusterEngine() string {
	if config.Cfg != nil && config.Cfg.ClusterEngine != "" {
		return config.Cfg.ClusterEngine
	}
	return "apple"
}

// momentGap returns the configured moment-segmentation time gap used by the
// apple engine's pass-1 greedy clustering. Falls back to 60 minutes when
// config isn't initialized or the value is non-positive. Now resolved
// through the 4-layer calibration stack (conf-explicit > calibrated state >
// profile default > code default; see resolveThreshold).
func momentGap() time.Duration {
	v, _ := resolveThreshold("MomentGapMinutes", 60)
	return time.Duration(v) * time.Minute
}

// tightEps returns the configured pass-1 greedy epsilon for the apple engine.
// Falls back to 0.35 when config isn't initialized or the value is
// non-positive. Now resolved through the 4-layer calibration stack
// (conf-explicit > calibrated state > profile default > code default; see
// resolveThreshold).
func tightEps() float64 {
	v, _ := resolveThreshold("ClusterTightEps", 0.35)
	return v
}

// mergeEps returns the configured pass-2 HAC stop distance for the apple
// engine. Falls back to 0.55 when config isn't initialized or the value is
// non-positive. Now resolved through the 4-layer calibration stack
// (conf-explicit > calibrated state > profile default > code default; see
// resolveThreshold).
func mergeEps() float64 {
	v, _ := resolveThreshold("ClusterMergeEps", 0.55)
	return v
}

// personAnchoredCond is the SQL predicate for persons that must survive
// re-clustering with identity intact: user-named / favorited / related /
// hidden, plus a user-pinned cover (cover_locked=1) or a user-chosen hero
// background. NOTE: purgeAutoPersons / purgeEmptyAutoPersons deliberately
// keep the narrower legacy predicate — those paths only run when a person
// has zero member faces left, and an unnamed shell whose only anchor was a
// (now dangling) pinned cover must still be cleaned up.
const personAnchoredCond = `(name!='' OR favorite=1 OR relation!='' OR hidden=1 OR cover_locked=1 OR COALESCE(hero_asset_id,'')!='')`

// cosDist computes the cosine distance between two float32 vectors.
// Returns 1.0 if either vector has zero norm.
func cosDist(a, b []float32) float64 {
	var dot, normA, normB float64
	for i := range a {
		ai := float64(a[i])
		bi := float64(b[i])
		dot += ai * bi
		normA += ai * ai
		normB += bi * bi
	}
	normA = math.Sqrt(normA)
	normB = math.Sqrt(normB)
	if normA == 0 || normB == 0 {
		return 1.0
	}
	cos := dot / (normA * normB)
	// Clamp to [-1, 1] to guard against floating-point overshoot.
	if cos > 1.0 {
		cos = 1.0
	} else if cos < -1.0 {
		cos = -1.0
	}
	return 1.0 - cos
}

// ComputeCentroid returns the per-dimension average (centroid) of a set of
// vectors. Returns nil for an empty set or mismatched dimensions; the caller
// must ensure all vectors have equal length.
func ComputeCentroid(vecs [][]float32) []float32 {
	if len(vecs) == 0 {
		return nil
	}
	dim := len(vecs[0])
	for _, v := range vecs {
		if len(v) != dim {
			return nil
		}
	}
	out := make([]float32, dim)
	for _, v := range vecs {
		for i := 0; i < dim; i++ {
			out[i] += v[i]
		}
	}
	n := float32(len(vecs))
	for i := range out {
		out[i] /= n
	}
	return out
}

// ClusterConfidence returns cluster cohesion in [0,1]: the average cosine
// similarity of members to the centroid. Returns 1.0 for a singleton cluster,
// 0.0 for an empty cluster.
func ClusterConfidence(vecs [][]float32, centroid []float32) float64 {
	if len(vecs) == 0 || centroid == nil {
		return 0.0
	}
	if len(vecs) == 1 {
		return 1.0
	}
	var sum float64
	for _, v := range vecs {
		sum += 1.0 - cosDist(v, centroid)
	}
	conf := sum / float64(len(vecs))
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}
	return conf
}

// regionQuery returns indices of all vectors (excluding idx itself) whose
// cosine distance to vecs[idx] is <= epsilon.
func regionQuery(vecs [][]float32, idx int, epsilon float64) []int {
	var neighbors []int
	for j := range vecs {
		if j == idx {
			continue
		}
		if cosDist(vecs[idx], vecs[j]) <= epsilon {
			neighbors = append(neighbors, j)
		}
	}
	return neighbors
}

// DBSCAN runs the DBSCAN clustering algorithm on vecs using cosine distance.
// epsilon is the maximum distance threshold; minPoints is the minimum number
// of neighbours (including the point itself) for a point to be a core point.
// Returns a label slice where label[i] >= 0 is the cluster ID for vecs[i].
// Noise points (unreachable from any core point) are assigned their own
// singleton cluster when minPoints == 1.
func DBSCAN(vecs [][]float32, epsilon float64, minPoints int) []int {
	n := len(vecs)
	labels := make([]int, n)
	for i := range labels {
		labels[i] = -1
	}
	visited := make([]bool, n)
	clusterID := 0

	for i := 0; i < n; i++ {
		if visited[i] {
			continue
		}
		visited[i] = true
		neighbors := regionQuery(vecs, i, epsilon)

		if len(neighbors) < minPoints {
			// Not enough neighbours: assign singleton cluster (handles minPoints==1 case
			// implicitly, but with minPoints==1 this branch is never reached because
			// regionQuery returns at least 0 items and minPoints-1 == 0).
			labels[i] = clusterID
			clusterID++
			continue
		}

		labels[i] = clusterID
		// Use a slice as a queue; iterate by index so appends are visible.
		seeds := make([]int, len(neighbors))
		copy(seeds, neighbors)

		for j := 0; j < len(seeds); j++ {
			s := seeds[j]
			if !visited[s] {
				visited[s] = true
				sNeighbors := regionQuery(vecs, s, epsilon)
				if len(sNeighbors) >= minPoints {
					for _, s2 := range sNeighbors {
						if !visited[s2] {
							seeds = append(seeds, s2)
						}
					}
				}
			}
			if labels[s] == -1 {
				labels[s] = clusterID
			}
		}
		clusterID++
	}

	return labels
}

// buildNeighborLists computes, for every vector, the sparse set of neighbor
// indices whose cosine distance is <= epsilon (self excluded). The O(n^2)
// distance work is split by row across runtime.NumCPU() workers; each worker
// only ever writes to the row range it owns (out[lo:hi]), so there is no
// shared-write contention and no need for per-row locking. The number of
// completed rows is aggregated with an atomic counter; onProgress itself is
// invoked under a mutex (held for the duration of the call) so callers never
// observe concurrent/overlapping invocations — matching the single-threaded
// delivery contract DBSCANWithProgress previously provided from its outer
// loop, on which existing progress callbacks (e.g. mutating shared slices or
// publishing SSE events) rely. onProgress fires once per 1% of rows
// completed. onProgress may be nil, in which case no progress is reported.
func buildNeighborLists(vecs [][]float32, epsilon float64, onProgress func(done, n int)) [][]int {
	n := len(vecs)
	out := make([][]int, n)
	if n == 0 {
		if onProgress != nil {
			onProgress(0, 0)
		}
		return out
	}

	workers := runtime.NumCPU()
	if workers > n {
		workers = n
	}
	if workers < 1 {
		workers = 1
	}

	var completed atomic.Int64
	var progressMu sync.Mutex
	lastReported := -1 // guarded by progressMu
	report := func() {
		done := completed.Add(1)
		if onProgress == nil {
			return
		}
		bucket := int((done * 100) / int64(n))
		progressMu.Lock()
		defer progressMu.Unlock()
		// Strict > (not !=) so a goroutine that reads a stale, already-
		// superseded bucket can never rewind lastReported and re-fire a
		// smaller "done" after a larger one was already reported.
		if bucket > lastReported {
			lastReported = bucket
			onProgress(int(done), n)
		}
	}

	rowsPerWorker := (n + workers - 1) / workers
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo := w * rowsPerWorker
		hi := lo + rowsPerWorker
		if hi > n {
			hi = n
		}
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			for i := lo; i < hi; i++ {
				var neighbors []int
				for j := 0; j < n; j++ {
					if j == i {
						continue
					}
					if cosDist(vecs[i], vecs[j]) <= epsilon {
						neighbors = append(neighbors, j)
					}
				}
				out[i] = neighbors
				report()
			}
		}(lo, hi)
	}
	wg.Wait()

	// Only fire the explicit trailing call when the last worker's report()
	// hasn't already pushed lastReported to the terminal 100% bucket —
	// otherwise the caller would observe two (n,n) terminal calls.
	progressMu.Lock()
	alreadyTerminal := lastReported == 100
	progressMu.Unlock()
	if onProgress != nil && !alreadyTerminal {
		onProgress(n, n) // guarantee the final state has done==n
	}
	return out
}

// DBSCANWithProgress behaves like DBSCAN, but calls onProgress once per 1%
// of progress crossed. onProgress must not be nil; the final call before
// return is guaranteed to have done==n.
//
// Phase 1 parallelizes the O(n^2) neighbor computation (buildNeighborLists)
// across CPU cores — this is where nearly all the wall-clock time goes, and
// where the progress reporting now lives. Phase 2 is the original serial
// cluster-expansion logic, unchanged line-for-line except that regionQuery
// calls are replaced with lookups into the precomputed neighbor lists; the
// outer loop order and seed-queue expansion order are preserved exactly, so
// labels are byte-identical to DBSCAN.
func DBSCANWithProgress(vecs [][]float32, epsilon float64, minPoints int, onProgress func(done, n int)) []int {
	n := len(vecs)
	neighborLists := buildNeighborLists(vecs, epsilon, onProgress)

	labels := make([]int, n)
	for i := range labels {
		labels[i] = -1
	}
	visited := make([]bool, n)
	clusterID := 0

	for i := 0; i < n; i++ {
		if visited[i] {
			continue
		}
		visited[i] = true
		neighbors := neighborLists[i]
		if len(neighbors) < minPoints {
			labels[i] = clusterID
			clusterID++
			continue
		}
		labels[i] = clusterID
		seeds := make([]int, len(neighbors))
		copy(seeds, neighbors)
		for j := 0; j < len(seeds); j++ {
			s := seeds[j]
			if !visited[s] {
				visited[s] = true
				sNeighbors := neighborLists[s]
				if len(sNeighbors) >= minPoints {
					for _, s2 := range sNeighbors {
						if !visited[s2] {
							seeds = append(seeds, s2)
						}
					}
				}
			}
			if labels[s] == -1 {
				labels[s] = clusterID
			}
		}
		clusterID++
	}
	return labels
}

// faceRow holds a single face detection record loaded from the database.
type faceRow struct {
	id      string
	assetID string
	vec     []float32
	// takenAt/indexedAt are the owning asset's capture/index timestamps, used
	// by the apple engine's SegmentMoments to bucket faces into moments. Zero
	// value when the underlying column is NULL (mirrors SegmentMoments'
	// "zero takenAt falls back to indexedAt" contract).
	takenAt   time.Time
	indexedAt time.Time
}

// FaceService handles face clustering and person management.
type FaceService struct {
	db      *sql.DB
	reg     *TaskRegistry
	running atomic.Bool

	// calibrating single-flights maybeCalibrate: a clustering pass that
	// finishes while a previous calibration run is still in flight (e.g. a
	// slow twopass grid scan on a large library) fires a second goroutine
	// that just observes the CAS fail and returns immediately, rather than
	// running two calibration passes concurrently against the same DB.
	calibrating atomic.Bool

	// ml is used by RunPipeline's detection stage to call
	// DetectAndRecognizeFaces; when not injected (nil), every detection fails
	// and takes the "skip, leave face_scanned=0 for the next retry" path.
	ml MLProvider
	// thumbDir is the thumbnail root directory: for video assets, the
	// detection input is <thumbDir>/<id>/large.jpg (a keyframe), falling back
	// to small.jpg when missing (mirrors the Indexer's thumbDir field).
	thumbDir string

	// markerDir is the directory one-shot migration marker files live in
	// (currently just the exemplar-assignment migration's
	// .exemplar_init_v1.done — see exemplar_migrate.go). Empty ("", the zero
	// value, e.g. every FaceService built without calling SetMarkerDir)
	// deliberately disables migration-awareness entirely: rebuildPersonsWithProgress's
	// step 1.5 always runs its normal (post-migration) detach behavior in
	// that case — exactly the pre-Task-7 behavior every existing revalidation
	// test already exercises, so those tests needed no changes.
	markerDir string

	// Failure backoff: after RunClustering errors, don't retrigger for a
	// while, to avoid a retry storm every minute.
	failMu      sync.Mutex
	nextAttempt time.Time

	// indexIdleFor returns "how long since the last indexing activity". The
	// safety-net trigger (scheduler) debounces on this, to avoid a momentary
	// gap in the index queue (pending==0 for an instant) during a large
	// upload being mistaken for the upload finishing and clustering firing
	// early. When nil (tests / not wired up), treated as always idle.
	indexIdleFor func() time.Duration

	// duePurger sweeps persons whose undo grace period has elapsed
	// (PersonService.PurgeDuePersons), injected because FaceService owns the
	// minute scheduler while person purge logic lives in PersonService.
	duePurger func(context.Context) error
}

// clusterFailBackoff is the minimum retry interval after RunClustering fails.
const clusterFailBackoff = 30 * time.Minute

// clusterQuietPeriod is the "indexing activity quiet duration" required
// before the safety net fires: indexing activity must have stopped for
// longer than this before the whole upload/index batch is considered truly
// finished. Must exceed the typical per-item processing interval during
// activity (ML takes roughly a few seconds per image).
const clusterQuietPeriod = 12 * time.Second

// NewFaceService creates a new FaceService backed by the given database.
func NewFaceService(db *sql.DB) *FaceService {
	return &FaceService{db: db}
}

// SetTaskRegistry injects a TaskRegistry so RunClustering can report progress.
func (s *FaceService) SetTaskRegistry(reg *TaskRegistry) { s.reg = reg }

// SetIndexIdleSource injects a callback returning the time since the last index
// activity, used to debounce the safety-net clustering trigger.
func (s *FaceService) SetIndexIdleSource(fn func() time.Duration) { s.indexIdleFor = fn }

// SetML injects the ML provider used by RunPipeline's detection stage.
func (s *FaceService) SetML(ml MLProvider) { s.ml = ml }

// SetDuePurger injects the callback that sweeps persons whose undo grace
// period has elapsed (PersonService.PurgeDuePersons), called on the minute
// scheduler tick right after ClearDanglingCovers.
func (s *FaceService) SetDuePurger(fn func(context.Context) error) { s.duePurger = fn }

// SetThumbDir injects the thumbnail root directory (mirrors the Indexer's
// existing field), used by RunPipeline to locate video keyframe thumbnails
// for detection.
func (s *FaceService) SetThumbDir(dir string) { s.thumbDir = dir }

// SetMarkerDir injects the directory used for one-shot migration marker
// files (see the markerDir field doc comment and exemplar_migrate.go).
// Mirrors SetThumbDir. Left unset in most tests.
func (s *FaceService) SetMarkerDir(dir string) { s.markerDir = dir }

// countUnassignedFaces returns the count of active (non-excluded) faces not
// yet associated with any person — i.e. faces newly uploaded/detected that
// haven't been swept by clustering yet. Also used as: (1) the "newly added
// this run" semantics of Task.Added after clustering finishes (so the
// frontend can toast only when there's something new, showing the added
// count rather than the total); (2) what RunPipeline checks to decide
// whether there's still work to do when there's nothing left to detect.
func (s *FaceService) countUnassignedFaces(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM face_detections fd
		JOIN assets a ON a.id = fd.asset_id
		WHERE fd.excluded = 0
		  AND NOT EXISTS (SELECT 1 FROM face_person fp WHERE fp.face_id = fd.id)`).Scan(&n)
	return n, err
}

// clusterStage runs one full clustering pass: load all face vectors, run
// DBSCAN, rebuild persons/face_person. pub receives this stage's local
// progress [0,1] (loading 0–0.10, DBSCAN 0.10–0.85, persisting 0.85–1.0);
// the caller maps that into its own global progress range as needed
// (RunClustering uses 0–1 as-is; RunPipeline folds it into the 95%–100%
// tail after detection completes). When total==0 (no faces at all in the
// DB, e.g. all photos were deleted), orphan persons are cleaned up and the
// function returns (0, 0, nil) as-is, calling neither onStart nor pub — the
// caller uses this to detect "nothing to publish as a task". onStart is
// called once after confirming total>0 and before real work starts (used to
// create the task / emit the first running frame).
func (s *FaceService) clusterStage(ctx context.Context, onStart func(total int64), pub func(p float64)) (total, newFaces int64, err error) {
	var t int64
	// Consistent with loadFacesWithProgress: only counts face_detections tied
	// to an existing asset.
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM face_detections fd
		JOIN assets a ON a.id = fd.asset_id
		WHERE fd.excluded = 0`).Scan(&t); err != nil {
		return 0, 0, err
	}
	if t == 0 {
		// No faces left (e.g. all photos were deleted): the non-anchored
		// persons produced by auto-clustering now have no members left and
		// must be cleaned up here, or the persons table will accumulate
		// orphans — this early return is exactly the path that
		// rebuildPersonsWithProgress's cleanup would otherwise skip.
		return 0, 0, s.purgeAutoPersons(ctx)
	}

	nf, _ := s.countUnassignedFaces(ctx)

	if onStart != nil {
		onStart(t)
	}

	faces, err := s.loadFacesWithProgress(ctx, t, func(loaded int64) {
		pub(0.10 * float64(loaded) / float64(t))
	})
	if err != nil {
		return 0, 0, err
	}

	vecs := make([][]float32, len(faces))
	for i, f := range faces {
		vecs[i] = f.vec
	}

	// Engine selection lives here (the sole call site), not inside
	// clusterEngine(): the accessor's job is to mirror config.Cfg, and its
	// siblings (momentGap/tightEps/mergeEps) are pure fallback-on-empty
	// readers with no logging side effect. Falling back an *invalid* (not
	// just empty) value to "apple" is a piece of orchestration policy that
	// only clusterStage's dispatch needs to know about, so it stays here,
	// next to the zap logger it needs to warn through.
	engine := clusterEngine()
	if engine != "dbscan" && engine != "apple" {
		zap.L().Warn("unknown photos.ClusterEngine value, falling back to apple",
			zap.String("configured", engine))
		engine = "apple"
	}

	// labels is only populated for "dbscan": it is computed here, over every
	// loaded face (anchored and free alike), exactly as before this engine
	// switch existed -- preserving byte-identical DBSCAN behavior. For
	// "apple", labels stays nil: the two-pass engine must never see an
	// already-anchored face mixed in with free ones (that reintroduces the
	// transitive-chaining risk HACComplete's docstring warns about), and the
	// anchored/free split isn't known until rebuildPersonsWithProgress's step
	// 3 runs inside the transaction -- so the real computation happens there,
	// against the free subset only.
	var labels []int
	switch engine {
	case "dbscan":
		labels = DBSCANWithProgress(vecs, clusterEpsilon(), dbscanMinPoints,
			func(done, n int) {
				if n == 0 {
					return
				}
				pub(0.10 + 0.75*float64(done)/float64(n))
			})
	default: // "apple"
		// Neither GreedyMomentClusters nor HACComplete expose incremental
		// progress (they're cheap enough over the pass-1 cluster counts this
		// runs against that a per-item callback isn't worth the API surface),
		// so this reports a few coarse ticks across the same [0.10, 0.85]
		// window DBSCANWithProgress uses above, keeping the
		// loading/clustering/persisting three-stage progress contract
		// callers rely on (e.g. TestRunClustering_StagesPublishProgress)
		// intact regardless of engine.
		pub(0.10 + 0.75/3)
		pub(0.10 + 0.75*2/3)
		pub(0.85)
	}

	if err := s.rebuildPersonsWithProgress(ctx, faces, labels, engine,
		func(done, n int) {
			if n == 0 {
				return
			}
			pub(0.85 + 0.15*float64(done)/float64(n))
		},
	); err != nil {
		return 0, 0, err
	}

	return t, nf, nil
}

// RunClustering reads all face embeddings, runs DBSCAN, and rebuilds the
// persons and face_person tables from scratch.
// Concurrent calls are safe: the second call returns nil immediately (CAS guard).
func (s *FaceService) RunClustering(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return nil
	}
	defer s.running.Store(false)

	taskID := fmt.Sprintf("face_%d", time.Now().UnixNano())
	started := time.Now()
	pub := func(progress float64, status string, errKey string, errParams map[string]string, total, newFaces int64) {
		if s.reg == nil {
			return
		}
		t := Task{
			ID:        taskID,
			Type:      "face",
			Label:     "Recognizing people",
			Progress:  progress,
			Status:    status,
			StartedAt: started,
		}
		if errKey != "" {
			t.SetError(errKey, errParams)
		}
		// Fill in current/total (total face count) and added (newly added
		// this run) only on terminal states. Leaving them unset on running
		// intermediate states avoids a throttled publish carrying a stray 0
		// to the frontend and making the number flicker.
		if status == "done" || status == "error" {
			t.Current = total
			t.Total = total
			t.Added = newFaces
		}
		s.reg.Upsert(t)
	}

	var taskStarted bool
	total, newFaces, err := s.clusterStage(ctx,
		func(int64) {
			taskStarted = true
			pub(0, "running", "", nil, 0, 0)
		},
		func(p float64) { pub(p, "running", "", nil, 0, 0) },
	)
	if err != nil {
		if taskStarted {
			pub(0, "error", TaskErrFaceClusterFailed, map[string]string{"detail": err.Error()}, 0, 0)
		}
		return err
	}
	if !taskStarted {
		// total==0: no faces; clusterStage has already silently cleaned up
		// orphan persons, so no task is published.
		return nil
	}

	pub(1, "done", "", nil, total, newFaces)
	go func() {
		time.Sleep(taskCleanupDelay)
		if s.reg != nil {
			s.reg.Remove(taskID)
		}
	}()
	return nil
}

// faceScanTarget is a single asset queued for RunPipeline's detection stage.
type faceScanTarget struct {
	id      string
	path    string
	isVideo bool
}

// queryFaceScanTargets lists assets awaiting face detection: indexed, not
// deleted, not offline, and not yet run through face detection
// (face_scanned=0).
func (s *FaceService) queryFaceScanTargets(ctx context.Context) ([]faceScanTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.id, a.file_path, COALESCE(a.mime_type,'') LIKE 'video/%'
		FROM assets a
		WHERE a.status = 'indexed' AND a.deleted_at IS NULL AND a.offline = 0 AND a.face_scanned = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []faceScanTarget
	for rows.Next() {
		var t faceScanTarget
		if err := rows.Scan(&t.id, &t.path, &t.isVideo); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// detectFaceScanTarget runs detection+recognition on a single asset and
// writes face_detections, setting face_scanned=1 on success. Images read the
// original file; videos read <thumbDir>/<id>/large.jpg (a keyframe), falling
// back to small.jpg when missing. A file read failure or ML call error both
// return an error — the caller skips the asset on this basis, leaving
// face_scanned=0 for the next RunPipeline retry, without interrupting the
// rest of the batch.
//
// When the asset is an image and the original's pixel count exceeds
// maxMLInputPixels (a safety margin under the immich-ml container's PIL
// 178.9MP hard limit — a real case in the wild was a 16320x12240=199.8MP
// Pexels photo), the already-generated large.jpg thumbnail is used in place
// of the original, to avoid a request that would otherwise always 500,
// face_scanned never getting set, and RunPipeline retrying the same image
// forever. When the thumbnail is also unavailable, this falls back to the
// existing failure path (return error, skip, leave for the next retry)
// rather than forcing the oversized original onto the ML service.
func (s *FaceService) detectFaceScanTarget(ctx context.Context, t faceScanTarget) error {
	data, err := resolveFaceScanSource(s.thumbDir, t.id, t.path, t.isVideo)
	if err != nil {
		return err
	}
	if s.ml == nil {
		return fmt.Errorf("ML provider not injected")
	}
	faces, err := s.ml.DetectAndRecognizeFaces(data)
	if err != nil {
		return err
	}
	// The same path can be overwritten with different content (checksum
	// change resets face_scanned=0): drop this asset's previous detections
	// first, or faces from the old content keep polluting clustering forever.
	// Mirrors rebuild.go's delete-before-rescan; face_person rows are removed
	// by the FK cascade. Runs only after a successful ML call so a transient
	// ML failure never wipes existing data.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM face_detections WHERE asset_id = ?`, t.id); err != nil {
		return err
	}
	insertFaceDetections(s.db, t.id, faces)
	if _, err := s.db.ExecContext(ctx, `UPDATE assets SET face_scanned = 1 WHERE id = ?`, t.id); err != nil {
		return err
	}
	return nil
}

// RunPipeline is the combined face detection + clustering task: it first
// detects faces one by one on assets with status='indexed' AND offline=0
// AND face_scanned=0 (real progress 0→95%), then folds in the clustering
// tail (95%→100%). Reentrancy is guarded by CAS (shares RunClustering's
// s.running); skipped outright when FacesEnabled is off; no task is
// published when there's nothing to detect and no unassigned faces (avoids
// a task flashing empty for an instant). On done, Added keeps the same
// "newly added face count" semantics for the frontend toast.
func (s *FaceService) RunPipeline(ctx context.Context) error {
	if config.Cfg != nil && !config.Cfg.FacesEnabled {
		return nil
	}
	if !s.running.CompareAndSwap(false, true) {
		return nil
	}
	defer s.running.Store(false)

	targets, err := s.queryFaceScanTargets(ctx)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		unassigned, uerr := s.countUnassignedFaces(ctx)
		if uerr != nil {
			return uerr
		}
		if unassigned == 0 {
			// This is the daily-skip guard: in steady state (nothing to
			// detect, nothing unassigned to cluster) the scheduled daily
			// RunPipeline exits here without ever reaching clusterStage, so
			// persons are not rebuilt and person UUIDs don't churn.
			// Verified by TestRunPipelineNoOpWhenNothingChanged.
			return nil
		}
	}

	taskID := fmt.Sprintf("face_%d", time.Now().UnixNano())
	started := time.Now()
	total := int64(len(targets))
	// pub's running intermediate state passes current/curTotal straight into
	// Task.Current/Total; done/error terminal states uniformly overwrite them
	// with total (assets awaiting detection) and added, with unchanged
	// semantics. The detection stage (0–95%) passes real current/curTotal;
	// the clustering tail (95–100%) passes 0/0 — because the frontend
	// NimoTaskBar prefers current/total to compute the percentage when
	// total>0, and by the tail stage processed already equals total, so
	// still filling both would make the still-running tail read as 100%.
	// Zeroing them makes the frontend fall back to the progress field
	// itself, so the real 95→100% climb is visible, matching RunClustering's
	// existing pub pattern.
	pub := func(progress float64, status string, errKey string, errParams map[string]string, current, curTotal, added int64) {
		if s.reg == nil {
			return
		}
		t := Task{
			ID:        taskID,
			Type:      "face",
			Label:     "Recognizing people",
			Progress:  progress,
			Status:    status,
			StartedAt: started,
		}
		if errKey != "" {
			t.SetError(errKey, errParams)
		}
		if status == "running" {
			t.Current = current
			t.Total = curTotal
		}
		if status == "done" || status == "error" {
			t.Current = total
			t.Total = total
			t.Added = added
		}
		s.reg.Upsert(t)
	}
	pub(0, "running", "", nil, 0, total, 0)

	var processed int64
	for _, tgt := range targets {
		if ctx.Err() != nil {
			break
		}
		if derr := s.detectFaceScanTarget(ctx, tgt); derr != nil {
			zap.L().Warn("face detection skipped, will retry next round",
				zap.String("asset_id", tgt.id), zap.Error(derr))
		}
		processed++
		frac := 0.0
		if total > 0 {
			frac = 0.95 * float64(processed) / float64(total)
		}
		pub(frac, "running", "", nil, processed, total, 0)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	_, newFaces, cerr := s.clusterStage(ctx, nil, func(p float64) {
		if total == 0 {
			// No detection targets (a pure clustering-tail scenario, e.g.
			// leftover unclustered faces from history): there's no 0–95%
			// detection stage to map into, so the clustering progress fills
			// the whole 0–100% span directly; current/curTotal are likewise
			// zeroed, for the reason noted above at pub's definition.
			pub(p, "running", "", nil, 0, 0, 0)
			return
		}
		pub(0.95+0.05*p, "running", "", nil, 0, 0, 0)
	})
	if cerr != nil {
		pub(0.95, "error", TaskErrPeopleRecognitionFailed, map[string]string{"detail": cerr.Error()}, processed, total, 0)
		return cerr
	}

	pub(1, "done", "", nil, processed, total, newFaces)
	go func() {
		time.Sleep(taskCleanupDelay)
		if s.reg != nil {
			s.reg.Remove(taskID)
		}
	}()
	return nil
}

// purgeAutoPersons deletes every person produced by auto-clustering
// (non-anchored: no name/favorite/relation/hidden) along with its
// face_person rows. Anchored persons (user-named/favorited/related/hidden)
// are kept. Used to clean up orphan persons in the 0-face case; the deletion
// order matches rebuildPersonsWithProgress.
func (s *FaceService) purgeAutoPersons(ctx context.Context) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `
		DELETE FROM face_person
		WHERE person_id IN (SELECT id FROM persons WHERE NOT (name!='' OR favorite=1 OR relation!='' OR hidden=1))`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `
		DELETE FROM persons
		WHERE NOT (name!='' OR favorite=1 OR relation!='' OR hidden=1)`); err != nil {
		return err
	}
	err = tx.Commit()
	return err
}

// purgeEmptyAutoPersons deletes any person that is "non-anchored (no
// name/favorite/relation/hidden) and has no member faces left".
//
// Used for orphan self-healing after photo deletion: deleting an asset
// cascades via FK to delete face_detections and face_person, but persons is
// not on that cascade chain, leaving behind an empty-shell orphan. The
// regular path that cleans up persons (rebuildPersons) only runs when
// RunClustering executes — deletion alone doesn't trigger clustering — so an
// independent periodic self-heal is needed.
//
// Safety: after a normal clustering pass, a non-anchored person always has
// its face_person members set within the same transaction, so the EXISTS
// subquery finds it non-empty and keeps it; this only cleans up shells that
// already have "no members left", and can be called repeatedly at any time.
// shouldClusterUnassigned decides whether to trigger one clustering pass
// (excluding the daily 03:xx scheduled one):
//   - only clusters once when unassigned faces > 0 and there are no pending
//     assets left (indexing has fully finished).
//
// Design: clustering only runs once after indexing has gone fully quiet, not
// triggered early while indexing is still in progress. There used to be an
// early trigger ("cluster immediately once unassigned faces >=
// clusterBatchSize"), which could cause a single upload to cluster once
// mid-index and again after settling (a double run: the user would see two
// "Recognizing people" entries), so it was removed. The tradeoff: even a
// very large upload has to wait for indexing to fully finish before
// clustering once (the completion notice lands a few seconds later), in
// exchange for merging into a single entry.
//
// Relies on pending==0 rather than the upload batch callback (SetOnBatchDone
// is based on ingestTracker's current>=total, which never reaches zero when
// the declared total overstates what was actually processed) — otherwise a
// small number of faces would never get clustered, persons would stay at 0
// forever, and the "Recognizing people" task would never appear.
func (s *FaceService) shouldClusterUnassigned(ctx context.Context) bool {
	// Debounce: indexing activity hasn't been quiet long enough (the whole
	// upload/index batch may still be in progress, just hitting a momentary
	// gap) -> don't trigger.
	if s.indexIdleFor != nil && s.indexIdleFor() < clusterQuietPeriod {
		return false
	}
	var unassigned int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM face_detections fd
		 WHERE fd.excluded = 0
		   AND NOT EXISTS (SELECT 1 FROM face_person fp WHERE fp.face_id = fd.id)`,
	).Scan(&unassigned); err != nil || unassigned == 0 {
		return false
	}
	var pending int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM assets WHERE status='pending'`).Scan(&pending); err == nil && pending == 0 {
		return true
	}
	return false
}

func (s *FaceService) purgeEmptyAutoPersons(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM persons
		WHERE NOT (name!='' OR favorite=1 OR relation!='' OR hidden=1)
		  AND NOT EXISTS (SELECT 1 FROM face_person fp WHERE fp.person_id = persons.id)`)
	return err
}

// ClearDanglingCovers nulls out persons.cover_face_id/cover_asset_id when the
// referenced face_detections row no longer exists. cover_face_id was added via
// ALTER TABLE and carries no FK, so deleting an asset (which cascades its
// face_detections away) leaves the pointer dangling and the face-thumbnail
// endpoint permanently 404ing. Runs on the minute scheduler next to
// purgeEmptyAutoPersons; safe to call repeatedly.
func (s *FaceService) ClearDanglingCovers(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE persons SET cover_face_id=NULL, cover_asset_id=NULL, cover_locked=0
		WHERE cover_face_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM face_detections fd WHERE fd.id = persons.cover_face_id)`)
	return err
}

func (s *FaceService) loadFacesWithProgress(ctx context.Context, total int64,
	onProgress func(int64),
) ([]faceRow, error) {
	// The JOIN with assets filters out orphan face_detections (rows whose
	// asset_id points at a deleted asset). Without the JOIN,
	// rebuildPersons would pick an orphan's asset_id for cover and trip the
	// persons.cover_asset_id REFERENCES assets(id) foreign key.
	//
	// Deliberately does not filter offline=1: face vector data is fully
	// retained on disk regardless (it doesn't depend on the original image
	// file), so clustering results are unrelated to offline status. Excluding
	// offline assets here would, once the removable drive is reinserted
	// (MountGuard flips offline back to 0), trigger a re-clustering pass that
	// churns person groupings (the same batch of faces gets kicked out of
	// clustering, then reassigned once reinserted, possibly landing on a
	// different person). Presentation-layer filtering is already done via
	// offline=0 in queries like persons.go / search.go; the clustering engine
	// itself keeps the stable semantics of "if the data is there, it
	// participates in clustering".
	rows, err := s.db.QueryContext(ctx, `
		SELECT fd.id, fd.asset_id, fd.embedding, a.taken_at, a.indexed_at
		FROM face_detections fd
		JOIN assets a ON a.id = fd.asset_id
		WHERE fd.excluded = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]faceRow, 0, total)
	var loaded int64
	lastReport := -1
	for rows.Next() {
		var f faceRow
		var blob []byte
		// taken_at/indexed_at are DATETIME columns that can be NULL (an asset
		// row inserted before indexing finishes, or a test fixture that never
		// sets them) -- scanned as sql.NullString and parsed explicitly
		// (same convention as parseSQLiteTime's other callers), rather than
		// relying on the driver's native TIMESTAMP scan, so a NULL cleanly
		// becomes faceRow's documented zero time.Time rather than an error.
		var takenAtStr, indexedAtStr sql.NullString
		if err := rows.Scan(&f.id, &f.assetID, &blob, &takenAtStr, &indexedAtStr); err != nil {
			return nil, err
		}
		f.vec = sqlite.DeserializeFloat32(blob)
		if t := parseSQLiteTime(takenAtStr); t != nil {
			f.takenAt = *t
		}
		if t := parseSQLiteTime(indexedAtStr); t != nil {
			f.indexedAt = *t
		}
		out = append(out, f)
		loaded++
		if total > 0 {
			if b := int(loaded * 100 / total); b != lastReport {
				onProgress(loaded)
				lastReport = b
			}
		}
	}
	onProgress(loaded)
	return out, rows.Err()
}

// resolveOpenSuggestion deletes any OPEN person_suggestions row for
// (personID, faceID) -- called from every "auto" decision inside
// rebuildPersonsWithProgress: step 1.5's revalidation (a member that had
// drifted into the gray zone, and got an open 'review' row, has since
// recovered into the auto band) and step 3's free-face assignment (a face
// that had an open 'join' row from an earlier pass now auto-joins directly).
// Both are the same root cause: the SYSTEM itself just settled, in the
// affirmative, the exact question that open suggestion row was asking a
// human -- "does this face belong here" -- and leaving the row open lets a
// human act on it long after it's moot:
//   - a stale 'review' row rejected later detaches an otherwise still-good
//     member AND permanently negates it (no un-negate surface exists), so
//     the face can never rejoin that person via KNN voting again;
//   - a stale 'join' row rejected later writes a person_negatives row for a
//     face that is simultaneously a confirmed-good member, which then gets
//     silently evicted by a LATER revalidation pass once Match's negation
//     filter strips the person's own exemplars out of that face's pool.
//
// DELETE, not a status flip to some third state: 'resolved' isn't a value
// the person_suggestions.status CHECK constraint accepts (only 'open'/
// 'accepted'/'rejected'), and more fundamentally an open row is an
// ephemeral machine-generated question, not a record worth preserving once
// the question no longer applies -- unlike a DECIDED (accepted/rejected)
// row, which is a real user decision and stays as an audit trail. The
// `status='open'` clause is the exact WHERE-guard mirror of every other
// write site in this function (see the 'review' UPSERT and 'join' INSERT's
// INVARIANT comments): a decided row is machinery-read-only, never touched.
func resolveOpenSuggestion(ctx context.Context, stmt *sql.Stmt, personID, faceID string) error {
	_, err := stmt.ExecContext(ctx, personID, faceID)
	return err
}

// engine selects how step 4 turns the free-face subset into labels:
// "dbscan" consumes the precomputed global `labels` (aligned to `faces`,
// indexed via freeFace.idx, exactly as before this parameter existed);
// anything else ("apple", already validated/warned-on by clusterStage's
// caller) runs the two-pass engine against the free subset itself, since
// `labels` is nil in that case (see clusterStage).
func (s *FaceService) rebuildPersonsWithProgress(ctx context.Context, faces []faceRow, labels []int, engine string,
	onProgress func(done, n int),
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	// 1. Load anchored persons (matching personAnchoredCond: named/favorited/
	//    related/hidden, or with a pinned cover / hero) and their current
	//    member faces.
	//
	//    ENGINE SPLIT: "dbscan" keeps the legacy centroid computation
	//    (unchanged below) feeding step 3's assignEpsilon snap -- a rollback
	//    to dbscan must get the whole old stack, not a partial mix with the
	//    exemplar matcher. "apple" instead loads each member face WITH its
	//    quality signals (score/frontality/sharpness) and confirmed flag,
	//    runs SelectExemplars per person, persists the exemplar flags, and
	//    collects the selected vectors for BuildExemplarIndex below; the
	//    centroid is no longer part of anchored-person matching for apple
	//    (recomputePersonStatsTx still maintains it, for display/merge-
	//    suggestion use).
	type anchor struct {
		id       string
		centroid []float32
	}
	anchorRows, err := tx.QueryContext(ctx,
		`SELECT id FROM persons WHERE `+personAnchoredCond)
	if err != nil {
		return err
	}
	var anchorIDs []string
	for anchorRows.Next() {
		var id string
		if err = anchorRows.Scan(&id); err != nil {
			anchorRows.Close()
			return err
		}
		anchorIDs = append(anchorIDs, id)
	}
	if cerr := anchorRows.Err(); cerr != nil {
		anchorRows.Close()
		return cerr
	}
	anchorRows.Close()

	anchored := map[string]bool{}                // set of face_id belonging to anchored persons
	anchors := make([]anchor, 0, len(anchorIDs)) // dbscan-only: centroid snap targets
	exemplarVecs := map[string][][]float32{}     // apple-only: person -> selected exemplar vectors

	// clearExemplarStmt/setExemplarStmt persist SelectExemplars' output
	// (clear-then-set within this tx, per person) -- apple-only, prepared
	// once and reused across the anchored-persons loop below AND by step
	// 1.5's post-revalidation exemplar recompute further down.
	var clearExemplarStmt, setExemplarStmt *sql.Stmt
	// resolveSuggestionStmt backs resolveOpenSuggestion (see its doc comment)
	// -- prepared once here so it's ready for BOTH step 1.5's "auto" case
	// (a reviving member) and step 3's "auto" case (a free face that
	// auto-joins directly), the same way clearExemplarStmt/setExemplarStmt
	// above are shared across both stages.
	var resolveSuggestionStmt *sql.Stmt
	// negatives/k/minVotes/autoDist/suggestDist are apple-only, loaded/computed
	// once here so both step 1.5 (revalidation, matches a member against its
	// OWN person's exemplars) and step 3 (free-face assignment, matches
	// against the full index) share identical thresholds and the same
	// negation set -- hoisted out of step 3's switch (where only the latter
	// used to live) because step 1.5 now needs them first.
	var negatives map[[2]string]bool
	var k, minVotes int
	var autoDist, suggestDist float64
	// lossless gates step 1.5's revalidation below into the one-shot
	// exemplar-assignment migration's "first pass" mode (see
	// exemplar_migrate.go): computed fresh from the marker file's
	// absence/presence on THIS call, never cached as service state (the
	// marker file on disk is the single source of truth for "has the
	// migration's first pass already happened" -- a cached bool would go
	// stale across process restarts). Stays false for the dbscan engine
	// (revalidation is an exemplar-era concept, see the ENGINE SPLIT
	// comments throughout this function) and whenever markerDir was never
	// configured (SetMarkerDir), which is every test that doesn't opt in.
	var lossless bool
	if engine != "dbscan" {
		clearExemplarStmt, err = tx.PrepareContext(ctx, `UPDATE face_person SET exemplar=0 WHERE person_id=?`)
		if err != nil {
			return err
		}
		defer clearExemplarStmt.Close()
		setExemplarStmt, err = tx.PrepareContext(ctx, `UPDATE face_person SET exemplar=1 WHERE face_id=?`)
		if err != nil {
			return err
		}
		defer setExemplarStmt.Close()
		resolveSuggestionStmt, err = tx.PrepareContext(ctx,
			`DELETE FROM person_suggestions WHERE person_id=? AND face_id=? AND status='open'`)
		if err != nil {
			return err
		}
		defer resolveSuggestionStmt.Close()

		// negatives: (person_id, face_id) pairs Match must exclude a person's
		// exemplars for -- loaded once per pass, not per face.
		negatives = map[[2]string]bool{}
		nr, nerr := tx.QueryContext(ctx, `SELECT person_id, face_id FROM person_negatives`)
		if nerr != nil {
			err = nerr
			return err
		}
		for nr.Next() {
			var negPerson, negFace string
			if err = nr.Scan(&negPerson, &negFace); err != nil {
				nr.Close()
				return err
			}
			negatives[[2]string{negPerson, negFace}] = true
		}
		if cerr := nr.Err(); cerr != nil {
			nr.Close()
			return cerr
		}
		nr.Close()

		k, minVotes = assignK(), assignMinVotes()
		autoDist, suggestDist = assignAutoDist(), assignSuggestDist()
		lossless = exemplarMigrationLosslessPass(s.markerDir)
	}

	for _, pid := range anchorIDs {
		if engine == "dbscan" {
			// Legacy path: only the embedding is needed to compute a centroid.
			fr, ferr := tx.QueryContext(ctx, `
				SELECT fd.embedding
				FROM face_person fp
				JOIN face_detections fd ON fd.id = fp.face_id
				JOIN assets a ON a.id = fd.asset_id
				WHERE fp.person_id = ? AND fd.excluded = 0`, pid)
			if ferr != nil {
				err = ferr
				return err
			}
			var vecs [][]float32
			for fr.Next() {
				var blob []byte
				if err = fr.Scan(&blob); err != nil {
					fr.Close()
					return err
				}
				vecs = append(vecs, sqlite.DeserializeFloat32(blob))
			}
			if cerr := fr.Err(); cerr != nil {
				fr.Close()
				return cerr
			}
			fr.Close()
			// Record anchored members.
			mr, merr := tx.QueryContext(ctx, `SELECT face_id FROM face_person WHERE person_id=?`, pid)
			if merr != nil {
				err = merr
				return err
			}
			for mr.Next() {
				var fid string
				if err = mr.Scan(&fid); err != nil {
					mr.Close()
					return err
				}
				anchored[fid] = true
			}
			if cerr := mr.Err(); cerr != nil {
				mr.Close()
				return cerr
			}
			mr.Close()
			anchors = append(anchors, anchor{id: pid, centroid: ComputeCentroid(vecs)})
			continue
		}

		// apple: load member faces along with the quality signals
		// SelectExemplars' hard gate needs. Column order here MUST match the
		// Scan order below exactly (face_id, embedding, score, frontality,
		// sharpness, confirmed) -- this single query also gives us the
		// anchored member set, unlike the dbscan branch above which needs a
		// second face_id-only query.
		cr, cerr := tx.QueryContext(ctx, `
			SELECT fd.id, fd.embedding, fd.score, fd.frontality, fd.sharpness, fp.confirmed
			FROM face_person fp
			JOIN face_detections fd ON fd.id = fp.face_id
			JOIN assets a ON a.id = fd.asset_id
			WHERE fp.person_id = ? AND fd.excluded = 0`, pid)
		if cerr != nil {
			err = cerr
			return err
		}
		var cands []exemplarCandidate
		vecByFace := map[string][]float32{}
		for cr.Next() {
			var faceID string
			var blob []byte
			var score, frontality, sharpness sql.NullFloat64
			var confirmedFlag int
			if err = cr.Scan(&faceID, &blob, &score, &frontality, &sharpness, &confirmedFlag); err != nil {
				cr.Close()
				return err
			}
			vec := sqlite.DeserializeFloat32(blob)
			cands = append(cands, exemplarCandidate{
				FaceID: faceID, Vec: vec, Score: score,
				Frontality: frontality, Sharpness: sharpness,
				Confirmed: confirmedFlag != 0,
			})
			vecByFace[faceID] = vec
			anchored[faceID] = true
		}
		if cerr := cr.Err(); cerr != nil {
			cr.Close()
			return cerr
		}
		cr.Close()

		minScore, minFront, minSharp := exemplarQualityGate()
		selected := SelectExemplars(cands, exemplarCap(), minScore, minFront, minSharp)

		// Persist exemplar flags: clear then set within this tx, even for a
		// hidden person -- hidden persons still participate in Match (same
		// reason step 5's centroid write-back below covers hidden persons
		// too: their up-to-date state must not go stale between passes).
		if _, err = clearExemplarStmt.ExecContext(ctx, pid); err != nil {
			return err
		}
		if len(selected) > 0 {
			vecs := make([][]float32, len(selected))
			for i, fid := range selected {
				if _, err = setExemplarStmt.ExecContext(ctx, fid); err != nil {
					return err
				}
				vecs[i] = vecByFace[fid]
			}
			exemplarVecs[pid] = vecs
		}
	}

	// 1.5. Per-pass revalidation of already-anchored, non-exempt members
	//      (apple-only -- revalidation is an exemplar-era concept; the
	//      dbscan path is a complete, untouched legacy stack per the ENGINE
	//      SPLIT comments above). This is the drift-killer for "members can
	//      enter but never leave": once a face auto-joined an anchored
	//      person in some earlier pass, nothing until now ever re-checked
	//      whether it still belongs. Every pass, for every anchored person,
	//      each CURRENT member face that is NOT confirmed=1, NOT the
	//      person's cover_locked cover face, and NOT on the person's hero
	//      asset is re-matched against THAT PERSON's OWN exemplar set alone
	//      -- never the global index -- because the question here is "do you
	//      still look like this person", not "does anyone else want you
	//      more" (that competition already happened once, when the face
	//      first joined; re-running it here would let an unrelated stronger
	//      match steal a legitimately-still-valid member out from under its
	//      own person).
	//
	//      ORDERING (load-bearing, do not reshuffle):
	//        a) must run AFTER step 1 above, so exemplarVecs holds this
	//           pass's freshly-selected templates -- revalidating against a
	//           stale template would be pointless;
	//        b) must run BEFORE step 3's BuildExemplarIndex call below, and
	//           must itself recompute+re-persist the exemplar set of any
	//           person it changed, so a just-detached face can never linger
	//           as a stale exemplar feeding that index;
	//        c) placement before step 2 (auto-person deletion) vs. after is
	//           otherwise arbitrary -- neither reads nor writes here overlap
	//           with step 2's non-anchored persons.
	if engine != "dbscan" {
		changedPersons := map[string]bool{} // persons that lost >=1 member -> exemplar recompute needed

		for _, pid := range anchorIDs {
			vecs := exemplarVecs[pid]
			if len(vecs) == 0 {
				// No gated exemplars at all -- nothing to revalidate
				// against. BuildExemplarIndex would produce an empty pool
				// and Match would always return "none", which must NOT be
				// read as "every member drifted" -- it would evict an
				// entire person whose whole membership merely failed the
				// quality gate (e.g. pre-gen4 rows with no signals yet).
				continue
			}

			var coverLocked int
			var coverFaceID, heroAssetID string
			if err = tx.QueryRowContext(ctx,
				`SELECT cover_locked, COALESCE(cover_face_id,''), COALESCE(hero_asset_id,'') FROM persons WHERE id=?`,
				pid).Scan(&coverLocked, &coverFaceID, &heroAssetID); err != nil {
				return err
			}

			mr, merr := tx.QueryContext(ctx, `
				SELECT fd.id, fd.embedding, fd.asset_id, fp.confirmed
				FROM face_person fp
				JOIN face_detections fd ON fd.id = fp.face_id
				JOIN assets a ON a.id = fd.asset_id
				WHERE fp.person_id = ? AND fd.excluded = 0`, pid)
			if merr != nil {
				err = merr
				return err
			}
			type revalMember struct {
				faceID, assetID string
				vec             []float32
				confirmed       int
			}
			var members []revalMember
			for mr.Next() {
				var m revalMember
				var blob []byte
				if err = mr.Scan(&m.faceID, &blob, &m.assetID, &m.confirmed); err != nil {
					mr.Close()
					return err
				}
				m.vec = sqlite.DeserializeFloat32(blob)
				members = append(members, m)
			}
			if cerr := mr.Err(); cerr != nil {
				mr.Close()
				return cerr
			}
			mr.Close()

			// Single-person index: reusing Match (rather than a bespoke
			// median-of-k helper) keeps identical dual-threshold/median/
			// per-person-floor semantics to free-face assignment for free.
			// With only one person in the pool, Match's plurality-vote and
			// negation-exclusion logic degenerate harmlessly (a lone
			// candidate always "wins" its own vote, with no competitor to
			// tie against), leaving exactly the median-of-k-nearest distance
			// check that "still looks like this person" needs.
			//
			// 1-exemplar-person edge: when this person has exactly one
			// exemplar and the member under test IS that exemplar, the pool
			// contains only its own vector, so the nearest (and only)
			// neighbor is itself at dist 0 -- always "auto", never removed.
			// This is intentional, not a bug to guard against: an exemplar
			// face trivially still looks like itself, and a single-template
			// person has nothing else to compare against anyway (see
			// TestRunClustering_AppleRevalidate_SoloExemplarSelfMatchSurvives).
			soloIndex := BuildExemplarIndex(map[string][][]float32{pid: vecs})

			// demoteToReview is the shared write path for "keep the
			// membership, queue an open 'review' suggestion" -- used both by
			// the ordinary gray-zone "suggest" decision below AND, during the
			// migration's lossless first pass, by what would otherwise be a
			// "none" detach (see the switch below and exemplar_migrate.go).
			demoteToReview := func(faceID string, dist float64) error {
				_, err := tx.ExecContext(ctx, `
					INSERT INTO person_suggestions(id, person_id, face_id, kind, score, status, created_at)
					VALUES(?, ?, ?, 'review', ?, 'open', ?)
					ON CONFLICT(person_id, face_id) DO UPDATE SET
						kind=excluded.kind, score=excluded.score, created_at=excluded.created_at
					WHERE person_suggestions.status='open'`,
					uuid.NewString(), pid, faceID, dist, time.Now())
				return err
			}

			// nearestExemplarDist is only needed by the migration's lossless
			// "none" branch below: Match() always zeroes its returned dist
			// when decision=="none" (nothing further needs it in the normal
			// detach path), but a lossless-demoted review suggestion still
			// needs a real, sortable score for the review queue -- so this
			// recomputes the plain nearest-exemplar cosine distance directly
			// against this person's own template vectors.
			nearestExemplarDist := func(vec []float32) float64 {
				best := math.Inf(1)
				for _, ev := range vecs {
					if d := cosDist(vec, ev); d < best {
						best = d
					}
				}
				return best
			}

			for _, m := range members {
				if m.confirmed != 0 {
					continue // user-confirmed members are never revalidated
				}
				if coverLocked != 0 && coverFaceID == m.faceID {
					continue // the user-pinned cover face is exempt
				}
				if heroAssetID != "" && m.assetID == heroAssetID {
					continue // any face on the user-chosen hero asset is exempt
				}

				_, dist, decision := soloIndex.Match(m.vec, m.faceID, negatives, k, minVotes, autoDist, suggestDist)
				switch decision {
				case "auto":
					// Back within this person's own auto band. Membership
					// itself needs no action, but a member that had drifted
					// into the gray zone on an earlier pass (and got an open
					// 'review' row) may have since recovered -- e.g. the
					// person's exemplar set improved, or this face's own
					// signal got re-detected. That open row is now moot: the
					// system just re-confirmed this member itself, so leaving
					// the row open would let a human reject it later and
					// silently punish a currently-good member (see
					// resolveOpenSuggestion's doc comment for the full
					// failure mode). CRITICAL fix, final whole-span review.
					if err = resolveOpenSuggestion(ctx, resolveSuggestionStmt, pid, m.faceID); err != nil {
						return err
					}
				case "suggest":
					// Gray zone: keep the membership but flag it for human
					// re-confirmation (kind='review', distinct from a
					// free-face 'join' suggestion). UPSERT, not DO NOTHING
					// -- unlike a 'join' proposal (offered once per pair), a
					// review suggestion's score should track this member's
					// latest drift measurement across passes; also
					// overwrites kind in case a stale row of a different
					// kind exists for this same (person_id, face_id) pair
					// (the UNIQUE index has no kind column).
					//
					// INVARIANT (also guarded at the 'join' INSERT below):
					// a decided row (status accepted/rejected) is never
					// silently reopened by machinery -- only a user
					// action reopens a decided suggestion. The DO UPDATE's
					// WHERE clause enforces this at the write site: it only
					// fires when the existing row is still 'open' (so
					// status itself never needs setting -- an open row
					// simply stays open), leaving a decided row's
					// status/decided_at untouched even if this same member
					// drifts back into the gray zone on a later pass. Most
					// reopen paths become unreachable once T6's accept/
					// reject endpoints land anyway (accept -> confirmed,
					// exempt from revalidation entirely; reject -> detached
					// + negated, no longer a member to revalidate), but the
					// invariant is enforced defensively regardless.
					if err = demoteToReview(m.faceID, dist); err != nil {
						return err
					}
				default: // "none": beyond suggestDist -- drift confirmed,
					// UNLESS this is the exemplar-assignment migration's
					// lossless first pass (see exemplar_migrate.go): the
					// migration's whole point is that an EXISTING member's
					// continued presence came from the old centroid-snap
					// behavior, never from real user confirmation, so an
					// algorithmic detach on the very first exemplar-engine
					// pass could silently drop a face before a human ever
					// gets a chance to look at it. Demote instead of detach --
					// same write path as the ordinary "suggest" case above --
					// and leave the membership, and `anchored`/`changedPersons`,
					// untouched.
					if lossless {
						if err = demoteToReview(m.faceID, nearestExemplarDist(m.vec)); err != nil {
							return err
						}
						continue
					}
					// Detach, NOT reject: this is an algorithmic re-check,
					// not a user decision, so no person_negatives row is
					// written -- that would permanently block the face from
					// ever re-joining this person via KNN voting, which a
					// mere auto-eviction hasn't earned (auto-removal !=
					// user denial). The face returns to the free pool for
					// THIS SAME pass: clearing it from `anchored` here makes
					// step 3 below treat it exactly like any other
					// unassigned face -- it may re-match a different
					// person, or fall through into step 4's two-pass
					// clustering.
					if _, err = tx.ExecContext(ctx, `DELETE FROM face_person WHERE face_id=?`, m.faceID); err != nil {
						return err
					}
					delete(anchored, m.faceID)
					changedPersons[pid] = true
				}
			}
		}

		// Recompute+re-persist the exemplar set of every person that lost a
		// member above, BEFORE step 3's BuildExemplarIndex call reads
		// exemplarVecs -- otherwise a just-detached face would linger as a
		// stale exemplar template for the rest of this pass. Mirrors step
		// 1's candidate query/SelectExemplars call above exactly, just
		// re-run against the post-removal membership.
		for pid := range changedPersons {
			cr, cerr := tx.QueryContext(ctx, `
				SELECT fd.id, fd.embedding, fd.score, fd.frontality, fd.sharpness, fp.confirmed
				FROM face_person fp
				JOIN face_detections fd ON fd.id = fp.face_id
				JOIN assets a ON a.id = fd.asset_id
				WHERE fp.person_id = ? AND fd.excluded = 0`, pid)
			if cerr != nil {
				err = cerr
				return err
			}
			var cands []exemplarCandidate
			vecByFace := map[string][]float32{}
			for cr.Next() {
				var faceID string
				var blob []byte
				var score, frontality, sharpness sql.NullFloat64
				var confirmedFlag int
				if err = cr.Scan(&faceID, &blob, &score, &frontality, &sharpness, &confirmedFlag); err != nil {
					cr.Close()
					return err
				}
				vec := sqlite.DeserializeFloat32(blob)
				cands = append(cands, exemplarCandidate{
					FaceID: faceID, Vec: vec, Score: score,
					Frontality: frontality, Sharpness: sharpness,
					Confirmed: confirmedFlag != 0,
				})
				vecByFace[faceID] = vec
			}
			if cerr := cr.Err(); cerr != nil {
				cr.Close()
				return cerr
			}
			cr.Close()

			minScore, minFront, minSharp := exemplarQualityGate()
			selected := SelectExemplars(cands, exemplarCap(), minScore, minFront, minSharp)

			if _, err = clearExemplarStmt.ExecContext(ctx, pid); err != nil {
				return err
			}
			delete(exemplarVecs, pid) // drop the stale entry even if selected is now empty
			if len(selected) > 0 {
				vecs := make([][]float32, len(selected))
				for i, fid := range selected {
					if _, err = setExemplarStmt.ExecContext(ctx, fid); err != nil {
						return err
					}
					vecs[i] = vecByFace[fid]
				}
				exemplarVecs[pid] = vecs
			}
		}
	}

	// 2. Delete auto persons (non-anchored) and their face_person rows.
	if _, err = tx.Exec(`
		DELETE FROM face_person
		WHERE person_id IN (SELECT id FROM persons WHERE NOT ` + personAnchoredCond + `)`); err != nil {
		return err
	}
	if _, err = tx.Exec(`
		DELETE FROM persons
		WHERE NOT ` + personAnchoredCond); err != nil {
		return err
	}

	// 3. Free faces = faces not in the anchored member set.
	//
	//    ENGINE SPLIT: "dbscan" snaps each free face onto the nearest
	//    anchored centroid within assignEpsilon -- the legacy path, kept
	//    byte-identical (an engine=dbscan rollback must get the whole old
	//    stack, not a partial mix with the exemplar matcher). "apple"
	//    instead matches each free face against the exemplar index built in
	//    step 1: "auto" joins the person immediately (confirmed=0); "suggest"
	//    queues an open person_suggestions row (idempotent across passes via
	//    ON CONFLICT DO NOTHING on the (person_id,face_id) unique index --
	//    an already-open row for the same pair is left untouched, and a
	//    previously-rejected pair never reaches "suggest" again once it's in
	//    person_negatives, since Match excludes a negated person's exemplars
	//    for that face entirely); "none" and "suggest" both leave the face in
	//    the free set for step 4's two-pass clustering -- a suggestion is
	//    advisory, not a membership, so the face still needs an auto-person
	//    home until a human accepts it.
	// Pre-compile the face_person INSERT, shared by steps 3 and 4.
	fpStmt, err := tx.PrepareContext(ctx, `INSERT INTO face_person(face_id, person_id) VALUES(?,?)`)
	if err != nil {
		return err
	}
	defer fpStmt.Close()

	type freeFace struct {
		face faceRow
		idx  int
	}
	var free []freeFace

	switch engine {
	case "dbscan":
		for i, f := range faces {
			if anchored[f.id] {
				continue
			}
			assigned := false
			bestDist := assignEpsilon
			bestAnchor := ""
			for _, an := range anchors {
				if an.centroid == nil {
					continue
				}
				d := cosDist(f.vec, an.centroid)
				if d <= bestDist {
					bestDist = d
					bestAnchor = an.id
				}
			}
			if bestAnchor != "" {
				if _, err = fpStmt.ExecContext(ctx, f.id, bestAnchor); err != nil {
					return err
				}
				assigned = true
			}
			if !assigned {
				free = append(free, freeFace{face: f, idx: i})
			}
		}

	default: // "apple"
		// negatives/k/minVotes/autoDist/suggestDist were already loaded/
		// computed once, above step 1's anchor loop, and are shared with
		// step 1.5's revalidation.
		ix := BuildExemplarIndex(exemplarVecs)

		for i, f := range faces {
			if anchored[f.id] {
				continue
			}
			personID, dist, decision := ix.Match(f.vec, f.id, negatives, k, minVotes, autoDist, suggestDist)
			switch decision {
			case "auto":
				if _, err = fpStmt.ExecContext(ctx, f.id, personID); err != nil {
					return err
				}
				// This face may already carry an open 'join' row from an
				// earlier pass (it landed in the gray zone for `personID`
				// before, but now clears the auto bar directly -- e.g. its
				// own signal improved, or personID's exemplars did). That
				// row is now moot: it just auto-joined, so a later reject on
				// the stale card would write a person_negatives row for a
				// face that is simultaneously a confirmed member of that
				// same person, which a subsequent revalidation pass would
				// then silently evict via Match's negation filter (see
				// resolveOpenSuggestion's doc comment). IMPORTANT fix, final
				// whole-span review.
				if err = resolveOpenSuggestion(ctx, resolveSuggestionStmt, personID, f.id); err != nil {
					return err
				}
				continue // joined immediately -- not part of the free set
			case "suggest":
				// INVARIANT (see step 1.5's 'review' UPSERT above for the
				// full explanation): a decided suggestion row is never
				// silently reopened by machinery. DO NOTHING already
				// satisfies this by construction -- an existing row for
				// this (person_id, face_id) pair, decided or not, is left
				// completely untouched; only a brand-new pair gets
				// inserted.
				if _, err = tx.ExecContext(ctx, `
					INSERT INTO person_suggestions(id, person_id, face_id, kind, score, status, created_at)
					VALUES(?, ?, ?, 'join', ?, 'open', ?)
					ON CONFLICT(person_id, face_id) DO NOTHING`,
					uuid.NewString(), personID, f.id, dist, time.Now()); err != nil {
					return err
				}
			}
			free = append(free, freeFace{face: f, idx: i})
		}
	}

	// 4. Group the remaining free faces into new auto persons.
	// cover_asset_id / cover_face_id are set uniformly by
	// recomputePersonStatsTx, so left unset in this INSERT.
	personStmt, err := tx.PrepareContext(ctx, `INSERT INTO persons(id, name, created_at, updated_at) VALUES(?, '', ?, ?)`)
	if err != nil {
		return err
	}
	defer personStmt.Close()

	// freeLabels is aligned to `free` (not to `faces`). For "dbscan", labels
	// was already computed over every face before the anchored/free split
	// existed (clusterStage, byte-identical to the historical behavior), so
	// it's still looked up by the face's original position (ff.idx). For
	// "apple", the two-pass engine runs now, for the first time, restricted
	// to vecs/times of the free faces only -- moment segmentation must never
	// be told about an already-anchored face's capture time, and pass-1/2
	// must never union or merge across an anchored face, or a free cluster
	// could get transitively chained onto one purely via an anchored
	// bystander whose own label is never even consulted.
	var freeLabels []int
	switch engine {
	case "dbscan":
		freeLabels = make([]int, len(free))
		for i, ff := range free {
			freeLabels[i] = labels[ff.idx]
		}
	default: // "apple"
		freeVecs := make([][]float32, len(free))
		freeTakenAt := make([]time.Time, len(free))
		freeIndexedAt := make([]time.Time, len(free))
		for i, ff := range free {
			freeVecs[i] = ff.face.vec
			freeTakenAt[i] = ff.face.takenAt
			freeIndexedAt[i] = ff.face.indexedAt
		}
		moments := SegmentMoments(freeTakenAt, freeIndexedAt, momentGap())
		pass1 := GreedyMomentClusters(freeVecs, moments, tightEps())
		freeLabels = HACComplete(freeVecs, pass1, mergeEps())
	}

	// autoMemberSets accumulates each newly created auto person's member
	// face ids/vectors as they're assigned below -- apple-only, feeds step
	// 6's merge-question generation without a second query, since step 4
	// already has every free face's vector in memory right here.
	var autoMemberSets map[string]*mcPerson
	if engine != "dbscan" {
		autoMemberSets = map[string]*mcPerson{}
	}

	labelToPersonID := map[int]string{}
	now := time.Now()
	for i, ff := range free {
		l := freeLabels[i]
		pid, ok := labelToPersonID[l]
		if !ok {
			pid = uuid.NewString()
			labelToPersonID[l] = pid
			if _, err = personStmt.ExecContext(ctx, pid, now, now); err != nil {
				return err
			}
		}
		if _, err = fpStmt.ExecContext(ctx, ff.face.id, pid); err != nil {
			return err
		}
		if autoMemberSets != nil {
			pm, ok := autoMemberSets[pid]
			if !ok {
				pm = &mcPerson{id: pid}
				autoMemberSets[pid] = pm
			}
			pm.faceIDs = append(pm.faceIDs, ff.face.id)
			pm.vecs = append(pm.vecs, ff.face.vec)
		}
	}

	// 5. Write back centroid/confidence/cover_face_id for every person
	//    (including hidden ones): hidden persons also need an up-to-date
	//    centroid, or the next clustering pass's snap stage would use a
	//    stale one.
	if err = s.recomputePersonStatsTx(ctx, tx); err != nil {
		return err
	}

	// 6. Generate this pass's cluster-merge question candidates (apple
	//    engine only -- see service/merge_questions.go and OVERVIEW.md's
	//    "Cluster-merge questions" section). Deliberately placed after step
	//    5 (not step 4): by this point every person this pass touches --
	//    anchored and freshly-created auto alike -- has its final
	//    membership/centroid/confidence already written back, so this
	//    stage's own fresh member-vector query (loadAnchoredMemberSets)
	//    sees this pass's fully-settled state rather than an
	//    in-progress one.
	if autoMemberSets != nil {
		autoPersons := make([]mcPerson, 0, len(autoMemberSets))
		for _, pm := range autoMemberSets {
			pm.centroid = ComputeCentroid(pm.vecs)
			autoPersons = append(autoPersons, *pm)
		}
		if err = generateMergeSuggestionsTx(ctx, tx, autoPersons, anchorIDs); err != nil {
			return err
		}
	}

	n := len(faces)
	onProgress(n, n)
	if err = tx.Commit(); err != nil {
		return err
	}
	// Only write the migration marker AFTER a successful commit -- writing
	// it any earlier (e.g. right after step 1.5's loop, before steps 2-5 run)
	// would risk marking the migration done while this pass's own lossless
	// demotions never actually persisted (a later error rolling back the
	// tx), which is exactly the silent-data-loss scenario the migration
	// exists to prevent. See exemplar_migrate.go.
	if lossless {
		writeExemplarMigrationMarker(s.markerDir)
	}

	// Fire self-calibration asynchronously now that this pass's persons/
	// face_person/calibration-relevant tables have all landed. This does
	// NOT hard-exclude a concurrent clustering pass (WAL gives it a
	// consistent read snapshot, calibration_state is written in one small
	// tx, and any values it applies are only picked up by resolveThreshold's
	// cache on the NEXT pass anyway), and it never triggers reclustering
	// itself -- no feedback loop. See calibrate_run.go's maybeCalibrate doc
	// comment for the full reasoning.
	//
	// The wiring guard is checked HERE, synchronously, rather than relying
	// solely on maybeCalibrate's own first-line guard: every test in this
	// package builds a FaceService directly and never calls
	// SetCalibrationDB, so calibrationDBWired() is false for the entire
	// lifetime of nearly every test -- checking it before spawning means
	// those tests never spawn a goroutine here at all, which matters
	// because a spawned-but-immediately-returning goroutine can still
	// outlive its own test (Go gives no ordering guarantee between an
	// orphaned goroutine and the next test's cleanup) and, if some later,
	// unrelated test happens to wire calibration in the meantime, race that
	// later test's globals against this test's already-closed db. Checking
	// synchronously, in the same goroutine as the clustering pass itself,
	// observes only contemporaneous state and never spawns anything when
	// there is nothing to do.
	if calibrationDBWired() && config.Cfg != nil {
		go s.maybeCalibrate(ctx)
	}

	return nil
}

// recomputePersonStatsTx recomputes centroid and confidence for every person in
// the transaction. Cover face/asset respect the cover_locked flag: when locked
// and the stored cover face is still in the active face set, only centroid and
// confidence are updated; if the locked face has been detached (excluded), the
// lock is cleared and cover is reselected by centroid distance.
func (s *FaceService) recomputePersonStatsTx(ctx context.Context, tx *sql.Tx) error {
	// Load all person IDs along with their lock state and current cover face.
	prows, err := tx.QueryContext(ctx,
		`SELECT id, cover_locked, COALESCE(cover_face_id,'') FROM persons`)
	if err != nil {
		return err
	}
	type personMeta struct {
		id          string
		locked      int
		coverFaceID string
	}
	var metas []personMeta
	for prows.Next() {
		var m personMeta
		if err = prows.Scan(&m.id, &m.locked, &m.coverFaceID); err != nil {
			prows.Close()
			return err
		}
		metas = append(metas, m)
	}
	if cerr := prows.Err(); cerr != nil {
		prows.Close()
		return cerr
	}
	prows.Close()

	// Prepare two UPDATE variants to avoid repeated SQL parsing in the loop.
	// coverStmt: update centroid + confidence + cover face/asset (cover not locked).
	coverStmt, err := tx.PrepareContext(ctx,
		`UPDATE persons SET centroid=?, confidence=?, cover_face_id=?, cover_asset_id=?, updated_at=? WHERE id=?`)
	if err != nil {
		return err
	}
	defer coverStmt.Close()

	// statsOnlyStmt: update centroid + confidence only (cover locked and still valid).
	statsOnlyStmt, err := tx.PrepareContext(ctx,
		`UPDATE persons SET centroid=?, confidence=?, updated_at=? WHERE id=?`)
	if err != nil {
		return err
	}
	defer statsOnlyStmt.Close()

	now := time.Now()
	for _, meta := range metas {
		// Load member faces along with the same quality/EXIF/aesthetic signals
		// recomputeOneCentroidTx uses, so selectCoverFace ranks covers
		// identically in both paths.
		fr, ferr := tx.QueryContext(ctx, `
			SELECT fd.id, fd.asset_id, fd.embedding, fd.bbox, fd.score, fd.frontality, fd.sharpness,
			       a.aesthetic_score, e.width, e.height
			FROM face_person fp
			JOIN face_detections fd ON fd.id = fp.face_id
			JOIN assets a ON a.id = fd.asset_id
			LEFT JOIN asset_exif e ON e.asset_id = fd.asset_id
			WHERE fp.person_id = ? AND fd.excluded = 0`, meta.id)
		if ferr != nil {
			return ferr
		}
		var faceIDs, assetIDs, bboxes []string
		var vecs [][]float32
		var aesScores []sql.NullFloat64
		var detScores, fronts, sharps []sql.NullFloat64
		var ws, hs []sql.NullInt64
		for fr.Next() {
			var fid, aid, bbox string
			var blob []byte
			var detScore, aesScore, front, sharp sql.NullFloat64
			var w, h sql.NullInt64
			if err = fr.Scan(&fid, &aid, &blob, &bbox, &detScore, &front, &sharp, &aesScore, &w, &h); err != nil {
				fr.Close()
				return err
			}
			faceIDs = append(faceIDs, fid)
			assetIDs = append(assetIDs, aid)
			bboxes = append(bboxes, bbox)
			vecs = append(vecs, sqlite.DeserializeFloat32(blob))
			aesScores = append(aesScores, aesScore)
			detScores = append(detScores, detScore)
			fronts = append(fronts, front)
			sharps = append(sharps, sharp)
			ws = append(ws, w)
			hs = append(hs, h)
		}
		if cerr := fr.Err(); cerr != nil {
			fr.Close()
			return cerr
		}
		fr.Close()

		if len(vecs) == 0 {
			if _, err = tx.ExecContext(ctx, `UPDATE persons SET cover_face_id=NULL,
				cover_asset_id=NULL, cover_locked=0, centroid=NULL, confidence=0,
				updated_at=? WHERE id=?`, now, meta.id); err != nil {
				return err
			}
			continue
		}
		centroid := ComputeCentroid(vecs)
		conf := ClusterConfidence(vecs, centroid)

		// Determine whether to preserve the locked cover face.
		if meta.locked == 1 && meta.coverFaceID != "" {
			lockedFaceValid := false
			for _, fid := range faceIDs {
				if fid == meta.coverFaceID {
					lockedFaceValid = true
					break
				}
			}
			if lockedFaceValid {
				// Cover is locked and the face is still active: only update stats.
				if _, err = statsOnlyStmt.ExecContext(ctx,
					sqlite.SerializeFloat32(centroid), conf, now, meta.id); err != nil {
					return err
				}
				continue
			}
			// Locked face was detached: clear the lock so cover gets reselected below.
			if _, err = tx.ExecContext(ctx,
				`UPDATE persons SET cover_locked=0 WHERE id=?`, meta.id); err != nil {
				return err
			}
		}

		// Hybrid-score selection, shared with recomputeOneCentroidTx (the
		// merge/detach/unlock path) so both paths rank covers identically.
		best := selectCoverFace(vecs, centroid, bboxes, aesScores, ws, hs, detScores, fronts, sharps)
		if _, err = coverStmt.ExecContext(ctx,
			sqlite.SerializeFloat32(centroid), conf, faceIDs[best], assetIDs[best], now, meta.id); err != nil {
			return err
		}
	}
	return nil
}

// StartScheduler runs a background goroutine that triggers RunPipeline
// (combined detection + clustering, see RunPipeline):
//   - once per hour at 03:xx (minute < 5), or
//   - when the number of unassigned faces reaches clusterBatchSize.
func (s *FaceService) StartScheduler(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				// Orphan person self-heal (independent of the clustering
				// throttle/switch): deleting a photo cascades to delete its
				// faces but not persons, so every minute this cleans up
				// non-anchored persons that have no members left, covering
				// every deletion path.
				if err := s.purgeEmptyAutoPersons(ctx); err != nil {
					zap.L().Warn("purge empty auto-persons failed", zap.Error(err))
				}

				if err := s.ClearDanglingCovers(ctx); err != nil {
					zap.L().Warn("clear dangling covers failed", zap.Error(err))
				}

				if s.duePurger != nil {
					if err := s.duePurger(ctx); err != nil {
						zap.L().Warn("purge due persons failed", zap.Error(err))
					}
				}

				// Failure backoff: don't retry for a while after the last failure.
				s.failMu.Lock()
				nextOK := s.nextAttempt
				s.failMu.Unlock()
				if !nextOK.IsZero() && t.Before(nextOK) {
					continue
				}

				if config.Cfg != nil && !config.Cfg.FacesEnabled {
					continue
				}

				shouldRun := false

				if t.Hour() == 3 && t.Minute() < 5 {
					shouldRun = true
				} else {
					shouldRun = s.shouldClusterUnassigned(ctx)
				}

				if shouldRun {
					if err := s.RunPipeline(ctx); err != nil {
						zap.L().Error("face pipeline failed", zap.Error(err))
						s.failMu.Lock()
						s.nextAttempt = time.Now().Add(clusterFailBackoff)
						s.failMu.Unlock()
					} else {
						s.failMu.Lock()
						s.nextAttempt = time.Time{}
						s.failMu.Unlock()
					}
				}
			}
		}
	}()
}
