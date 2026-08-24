// Command cluster-analysis is an OFFLINE, READ-ONLY parameter study over a
// copy of the production Photos database, reusing the exact production
// DBSCAN/centroid/confidence logic (service.DBSCAN, service.DBSCANWithProgress,
// service.ComputeCentroid, service.ClusterConfidence) plus a locally
// reimplemented matrix-backed DBSCAN variant for fast parameter sweeps. It
// writes nothing to the DB (the DSN is opened with mode=ro) and is not wired
// into any build/deploy path (not referenced by any other package, not part
// of any `go build ./...` deploy target).
//
// See README.md in this directory for usage, the eps x minPoints sweep
// findings, and the latest production-copy validation numbers.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/NimoTech/NimoOS-Photos/service"
)

// garbageClusterPersonID is the known production mega-cluster (unnamed,
// confidence 0.30, 2612/4409 faces in the reference DB copy).
const garbageClusterPersonID = "5e128740-0b4e-4e65-a6de-fb41dd54d2c5"

// calibEpsilon/calibMinPoints are the historical production values (the
// legacy dbscanEpsilon/dbscanMinPoints constants in service/persons.go)
// used purely as a ground-truth calibration point: production's
// face_person table for the reference DB copy was built at eps=0.60,
// minPoints=1, and is known to contain a 2612-face mega cluster.
const (
	calibEpsilon   = 0.60
	calibMinPoints = 1
)

type Face struct {
	ID      string
	AssetID string
	Vec     []float32
	MinSide float64 // bbox min(width,height) in px
	HasExif bool
	ImgMinD float64 // min(width,height) of the image, px (only valid if HasExif)
	// TakenAt/IndexedAt are the owning asset's capture/index timestamps, used
	// by -mode twopass's service.SegmentMoments call. Zero value when the
	// underlying column is NULL (same convention as service's faceRow /
	// parseSQLiteTime: NULL -> zero time.Time, never an error).
	TakenAt   time.Time
	IndexedAt time.Time
}

type namedPerson struct {
	id, name string
	faceIDs  []string
}

type bboxJSON struct {
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
	X2 float64 `json:"x2"`
	Y2 float64 `json:"y2"`
}

// cosDist reimplements service's unexported cosDist (cosine distance, clamped).
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
	if cos > 1.0 {
		cos = 1.0
	} else if cos < -1.0 {
		cos = -1.0
	}
	return 1.0 - cos
}

// parseSQLiteTime reimplements service's unexported parseSQLiteTime (in
// persons.go): parses a TEXT timestamp written by GORM (RFC3339 with offset,
// or the legacy "2006-01-02 15:04:05.000000-07:00" form), returning nil for
// a NULL/empty/unparseable column so callers can cleanly fall back to a zero
// time.Time -- the same NULL-handling convention as service's faceRow.
func parseSQLiteTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s.String); err == nil {
			return &t
		}
	}
	return nil
}

func mustOpenRO(dbPath string) *sql.DB {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	if err := db.Ping(); err != nil {
		log.Fatalf("ping db: %v", err)
	}
	return db
}

func loadFaces(db *sql.DB) []Face {
	rows, err := db.Query(`
		SELECT fd.id, fd.asset_id, fd.embedding, fd.bbox, a.taken_at, a.indexed_at
		FROM face_detections fd
		JOIN assets a ON a.id = fd.asset_id
		WHERE fd.excluded = 0`)
	if err != nil {
		log.Fatalf("load faces: %v", err)
	}
	defer rows.Close()

	var faces []Face
	for rows.Next() {
		var f Face
		var blob []byte
		var bboxStr string
		var takenAtStr, indexedAtStr sql.NullString
		if err := rows.Scan(&f.ID, &f.AssetID, &blob, &bboxStr, &takenAtStr, &indexedAtStr); err != nil {
			log.Fatalf("scan face: %v", err)
		}
		if t := parseSQLiteTime(takenAtStr); t != nil {
			f.TakenAt = *t
		}
		if t := parseSQLiteTime(indexedAtStr); t != nil {
			f.IndexedAt = *t
		}
		f.Vec = sqlite.DeserializeFloat32(blob)
		var bb bboxJSON
		if err := json.Unmarshal([]byte(bboxStr), &bb); err == nil {
			w := bb.X2 - bb.X1
			h := bb.Y2 - bb.Y1
			if w < 0 {
				w = -w
			}
			if h < 0 {
				h = -h
			}
			f.MinSide = math.Min(w, h)
		}
		faces = append(faces, f)
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("rows err: %v", err)
	}

	// Join asset_exif for image min-dimension.
	exif := map[string][2]float64{}
	erows, err := db.Query(`SELECT asset_id, width, height FROM asset_exif WHERE width IS NOT NULL AND height IS NOT NULL`)
	if err != nil {
		log.Fatalf("load exif: %v", err)
	}
	for erows.Next() {
		var aid string
		var w, h float64
		if err := erows.Scan(&aid, &w, &h); err != nil {
			log.Fatalf("scan exif: %v", err)
		}
		exif[aid] = [2]float64{w, h}
	}
	erows.Close()

	for i := range faces {
		if wh, ok := exif[faces[i].AssetID]; ok && wh[0] > 0 && wh[1] > 0 {
			faces[i].HasExif = true
			faces[i].ImgMinD = math.Min(wh[0], wh[1])
		}
	}
	return faces
}

// buildDistMatrix computes the full NxN cosine-distance matrix, parallelized
// across rows. Returns a flat []float32 (row-major, N*N).
func buildDistMatrix(vecs [][]float32) []float32 {
	n := len(vecs)
	mat := make([]float32, n*n)
	var wg sync.WaitGroup
	workers := runtime.NumCPU()
	rowsPerWorker := (n + workers - 1) / workers
	for w := 0; w < workers; w++ {
		start := w * rowsPerWorker
		end := start + rowsPerWorker
		if end > n {
			end = n
		}
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			for i := start; i < end; i++ {
				for j := i + 1; j < n; j++ {
					d := float32(cosDist(vecs[i], vecs[j]))
					mat[i*n+j] = d
					mat[j*n+i] = d
				}
			}
		}(start, end)
	}
	wg.Wait()
	return mat
}

// dbscanSubset is the matrix-backed variant of service.DBSCAN: same
// algorithm/tie-breaking, but regionQuery reads from the precomputed
// distance matrix instead of recomputing cosDist. indices is the set of
// global face indices to cluster (for the full-set run, 0..N-1); mat/N
// describe the full precomputed matrix (dist between global indices gi,gj
// is mat[gi*N+gj]). Returns labels aligned with indices (labels[k] is the
// cluster of face indices[k]).
func dbscanSubset(indices []int, mat []float32, N int, epsilon float64, minPoints int) []int {
	m := len(indices)
	labels := make([]int, m)
	for i := range labels {
		labels[i] = -1
	}
	visited := make([]bool, m)
	clusterID := 0
	eps := float32(epsilon)

	regionQuery := func(li int) []int {
		gi := indices[li]
		base := gi * N
		var nb []int
		for lj := 0; lj < m; lj++ {
			if lj == li {
				continue
			}
			if mat[base+indices[lj]] <= eps {
				nb = append(nb, lj)
			}
		}
		return nb
	}

	for i := 0; i < m; i++ {
		if visited[i] {
			continue
		}
		visited[i] = true
		neighbors := regionQuery(i)
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
				sNeighbors := regionQuery(s)
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

func clusterSizes(labels []int) map[int]int {
	sizes := map[int]int{}
	for _, l := range labels {
		sizes[l]++
	}
	return sizes
}

func maxClusterSize(sizes map[int]int) (label, size int) {
	for l, s := range sizes {
		if s > size {
			size = s
			label = l
		}
	}
	return
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

func distStats(vals []float64) (min, p10, median, p90, max, mean float64) {
	if len(vals) == 0 {
		return
	}
	s := append([]float64{}, vals...)
	sort.Float64s(s)
	min = s[0]
	max = s[len(s)-1]
	p10 = percentile(s, 0.10)
	median = percentile(s, 0.50)
	p90 = percentile(s, 0.90)
	var sum float64
	for _, v := range s {
		sum += v
	}
	mean = sum / float64(len(s))
	return
}

// loadNamedPersons queries every named (non-anonymous) person and their
// member face IDs, as the shared ground truth used by both the legacy
// eps/minPoints study and -mode twopass's grid scan.
func loadNamedPersons(db *sql.DB) []namedPerson {
	var named []namedPerson
	nrows, err := db.Query(`SELECT id, name FROM persons WHERE name != '' ORDER BY name`)
	if err != nil {
		log.Fatalf("query named persons: %v", err)
	}
	for nrows.Next() {
		var np namedPerson
		if err := nrows.Scan(&np.id, &np.name); err != nil {
			log.Fatalf("scan named person: %v", err)
		}
		named = append(named, np)
	}
	nrows.Close()
	for i := range named {
		named[i].faceIDs = queryPersonFaceIDs(db, named[i].id)
	}
	return named
}

func main() {
	dbPathFlag := flag.String("db", "", "path to a READ-ONLY copy of the Photos sqlite DB (required; opened with mode=ro, never written)")
	epsFlag := flag.Float64("eps", 0.48, "DBSCAN cosine-distance epsilon to validate (production default since C1 is 0.48; see pkg/config ClusterEpsilon)")
	minPtsFlag := flag.Int("minpts", 1, "DBSCAN minPoints to validate")
	minConfFlag := flag.Float64("minconf", 0.5, "MinPersonConfidence gate to compare the residual cluster's confidence against (production default)")
	modeFlag := flag.String("mode", "", `analysis mode: "" (default, legacy eps x minPoints DBSCAN calibration study), "twopass" (Task 6: two-pass Apple-engine T_tight x T_merge grid scan across gap in {30,60,120}min), "knn" (KNN exemplar-assignment T_auto x T_suggest calibration from confirmed/negative ground truth), or "merge" (T-merge cluster-merge cut-point calibration from decided merge_suggestions rows)`)
	knnKFlag := flag.Int("knnk", 5, "AssignKNNK: number of nearest exemplars used per KNN median-distance computation in -mode knn (production default per pkg/config AssignKNNK)")
	flag.Parse()

	if *dbPathFlag == "" {
		fmt.Println("usage: cluster-analysis -db <path-to-readonly-photos.db-copy> [-eps 0.48] [-minpts 1] [-minconf 0.5] [-mode twopass|knn|merge] [-knnk 5]")
		log.Fatal("-db is required")
	}

	db := mustOpenRO(*dbPathFlag)
	defer db.Close()

	fmt.Println("=== Loading faces (excluded=0, joined to live assets) ===")
	faces := loadFaces(db)
	N := len(faces)
	fmt.Printf("Loaded %d faces\n", N)

	idOf := make(map[string]int, N)
	for i, f := range faces {
		idOf[f.ID] = i
	}

	named := loadNamedPersons(db)
	fmt.Println("Named persons (ground truth):")
	for _, np := range named {
		fmt.Printf("  %-10s %s  faces=%d\n", np.name, np.id, len(np.faceIDs))
	}

	if *modeFlag == "twopass" {
		runTwoPass(faces, named)
		fmt.Println("\n=== DONE ===")
		return
	}

	if *modeFlag == "knn" {
		runKNN(os.Stdout, db, *knnKFlag)
		fmt.Println("\n=== DONE ===")
		return
	}

	if *modeFlag == "merge" {
		runMerge(os.Stdout, db)
		fmt.Println("\n=== DONE ===")
		return
	}

	// --- Ground truth: garbage cluster (twopass mode above doesn't need it) ---
	garbageFaceIDs := queryPersonFaceIDs(db, garbageClusterPersonID)
	fmt.Printf("Garbage cluster (%s) member faces: %d\n", garbageClusterPersonID, len(garbageFaceIDs))

	fmt.Println("\n=== Building full pairwise cosine-distance matrix ===")
	vecs := make([][]float32, N)
	for i, f := range faces {
		vecs[i] = f.Vec
	}
	mat := buildDistMatrix(vecs)
	fmt.Printf("Matrix built: %d x %d (%d bytes)\n", N, N, len(mat)*4)

	fullIdx := make([]int, N)
	for i := range fullIdx {
		fullIdx[i] = i
	}

	// --- Calibration gate 1: eps=0.60, minPts=1 must recover a ~2612-face
	// mega cluster (production's historical ground truth for this DB copy).
	fmt.Printf("\n=== CALIBRATION 1: eps=%.2f minPts=%d vs production ground truth (expect ~2612-face mega cluster) ===\n", calibEpsilon, calibMinPoints)
	calLabels := dbscanSubset(fullIdx, mat, N, calibEpsilon, calibMinPoints)
	calSizes := clusterSizes(calLabels)
	_, calMax := maxClusterSize(calSizes)
	fmt.Printf("Matrix-backed dbscanSubset: #clusters=%d maxClusterSize=%d (production garbage cluster=2612 faces, but note production's\n", len(calSizes), calMax)
	fmt.Println("face_person table reflects DBSCAN labels MINUS anchored/named faces snapped out via assignEpsilon=0.55 pre-pass,")
	fmt.Println("so an exact match is not expected -- this checks the raw DBSCAN labeling reproduces a comparably-sized mega cluster.")

	// --- Calibration gate 2: the local matrix-backed dbscanSubset must be
	// partition-identical to the real production service.DBSCAN (not just
	// "similarly sized") at both the historical eps and the eps under test.
	// This is what lets the sweep/validation numbers below be trusted as
	// faithful to production behavior rather than an artifact of the local
	// reimplementation.
	fmt.Println("\n=== CALIBRATION 2: matrix-backed dbscanSubset vs real service.DBSCAN (must be partition-identical) ===")
	assertMatchesServiceDBSCAN(vecs, calibEpsilon, calibMinPoints, calLabels)
	targetLabels := dbscanSubset(fullIdx, mat, N, *epsFlag, *minPtsFlag)
	assertMatchesServiceDBSCAN(vecs, *epsFlag, *minPtsFlag, targetLabels)

	// --- D2 timing: service.DBSCAN (serial regionQuery) vs
	// service.DBSCANWithProgress (D2's parallel neighbor-list precompute),
	// on the real production embeddings at the eps/minPoints under test.
	fmt.Printf("\n=== D2 TIMING (real production embeddings, eps=%.2f minPts=%d, %d CPUs) ===\n", *epsFlag, *minPtsFlag, runtime.NumCPU())
	timeD2Speedup(vecs, *epsFlag, *minPtsFlag)

	// ================= Q1: chaining anatomy =================
	fmt.Println("\n=== Q1: Chaining anatomy ===")
	analyzeChaining(faces, idOf, garbageFaceIDs, named, mat, N)

	// ================= Q2: epsilon sweep =================
	fmt.Println("\n=== Q2: epsilon x minPoints sweep ===")
	sweepResults := epsilonSweep(idOf, named, garbageFaceIDs, mat, N, fullIdx)
	printSweepTable(sweepResults)
	fmt.Println("\nSanity-check per-person detail (verifying purity=1.000 is not a bug):")
	printPerPersonDetail(sweepResults, 0.60, 1)
	printPerPersonDetail(sweepResults, 0.40, 1)
	printPerPersonDetail(sweepResults, 0.46, 2)
	printPerPersonDetail(sweepResults, 0.60, 2)
	printPerPersonDetail(sweepResults, 0.60, 3)

	// ================= Q3: size-gate simulation =================
	fmt.Println("\n=== Q3: bbox size-gate simulation ===")
	gateSimulation(faces, idOf, named, garbageFaceIDs, mat, N, sweepResults)

	// ================= Final validation: target eps/minPts config =================
	fmt.Printf("\n=== FINAL VALIDATION: eps=%.2f minPts=%d ===\n", *epsFlag, *minPtsFlag)
	validateFinalConfig(faces, idOf, vecs, named, garbageFaceIDs, targetLabels, *minConfFlag)

	fmt.Println("\n=== DONE ===")
}

// assertMatchesServiceDBSCAN runs the real production service.DBSCAN over
// vecs at (epsilon, minPoints) and fatally aborts if its cluster partition
// differs from wantLabels (already computed by the local matrix-backed
// dbscanSubset). Partition equality is checked up to cluster-ID relabeling
// (both algorithms iterate points in the same order with the same
// tie-breaking, so IDs are expected to match too, but only membership is
// asserted to keep the check robust).
func assertMatchesServiceDBSCAN(vecs [][]float32, epsilon float64, minPoints int, wantLabels []int) {
	start := time.Now()
	gotLabels := service.DBSCAN(vecs, epsilon, minPoints)
	elapsed := time.Since(start)
	wantSizes := clusterSizes(wantLabels)
	gotSizes := clusterSizes(gotLabels)
	_, wantMax := maxClusterSize(wantSizes)
	_, gotMax := maxClusterSize(gotSizes)
	if !partitionsEqual(partitionSignature(wantLabels), partitionSignature(gotLabels)) {
		log.Fatalf("CALIBRATION FAILED at eps=%.2f minPts=%d: matrix-backed dbscanSubset partition differs from service.DBSCAN "+
			"(dbscanSubset: #clusters=%d maxSize=%d; service.DBSCAN: #clusters=%d maxSize=%d, took %v)",
			epsilon, minPoints, len(wantSizes), wantMax, len(gotSizes), gotMax, elapsed)
	}
	fmt.Printf("PASS eps=%.2f minPts=%d: dbscanSubset and service.DBSCAN agree exactly (#clusters=%d maxSize=%d, service.DBSCAN took %v)\n",
		epsilon, minPoints, len(gotSizes), gotMax, elapsed)
}

// partitionSignature returns the cluster membership partition of labels as a
// canonically-ordered list of sorted index groups, suitable for equality
// comparison independent of cluster-ID numbering.
func partitionSignature(labels []int) [][]int {
	groups := map[int][]int{}
	for i, l := range labels {
		groups[l] = append(groups[l], i)
	}
	sigs := make([][]int, 0, len(groups))
	for _, g := range groups {
		sort.Ints(g)
		sigs = append(sigs, g)
	}
	sort.Slice(sigs, func(i, j int) bool {
		if len(sigs[i]) != len(sigs[j]) {
			return len(sigs[i]) < len(sigs[j])
		}
		for k := range sigs[i] {
			if sigs[i][k] != sigs[j][k] {
				return sigs[i][k] < sigs[j][k]
			}
		}
		return false
	})
	return sigs
}

func partitionsEqual(a, b [][]int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

// timeD2Speedup times the real production service.DBSCAN (serial
// regionQuery) against service.DBSCANWithProgress (D2's parallel
// neighbor-list precompute) over the same production embeddings, and prints
// the observed speedup as a cross-check of the D2 report's benchmark (which
// used synthetic random vectors of the same shape).
func timeD2Speedup(vecs [][]float32, epsilon float64, minPoints int) {
	t0 := time.Now()
	serialLabels := service.DBSCAN(vecs, epsilon, minPoints)
	serialElapsed := time.Since(t0)

	t1 := time.Now()
	parallelLabels := service.DBSCANWithProgress(vecs, epsilon, minPoints, func(done, n int) {})
	parallelElapsed := time.Since(t1)

	if !partitionsEqual(partitionSignature(serialLabels), partitionSignature(parallelLabels)) {
		log.Fatalf("D2 TIMING: service.DBSCAN and service.DBSCANWithProgress disagree at eps=%.2f minPts=%d -- this would be a D2 regression, not a benchmarking artifact", epsilon, minPoints)
	}
	speedup := float64(serialElapsed) / float64(parallelElapsed)
	fmt.Printf("serial   service.DBSCAN              = %v\n", serialElapsed)
	fmt.Printf("parallel service.DBSCANWithProgress   = %v\n", parallelElapsed)
	fmt.Printf("speedup                               = %.2fx (labels identical)\n", speedup)
}

// validateFinalConfig prints the C2 "final validation" numbers for the
// eps/minPoints combo under test: max cluster size, named-person purity, and
// the ClusterConfidence of the residual largest cluster compared against the
// MinPersonConfidence gate (does B7's cohesion floor hide it from listing?).
func validateFinalConfig(faces []Face, idOf map[string]int, vecs [][]float32, named []namedPerson, garbageFaceIDs []string, labels []int, minConf float64) {
	sizes := clusterSizes(labels)
	maxLbl, maxSz := maxClusterSize(sizes)
	fmt.Printf("maxClusterSize=%d (#clusters=%d)\n", maxSz, len(sizes))

	// Named-person purity: for every named person, what fraction of the
	// majority cluster they land in is actually them (same definition as
	// the Q2 sweep's meanPurity, printed per-person for full auditability).
	var puritySum float64
	var purityN int
	for _, np := range named {
		labelCount := map[int]int{}
		total := 0
		for _, fid := range np.faceIDs {
			gi, ok := idOf[fid]
			if !ok {
				continue
			}
			total++
			labelCount[labels[gi]]++
		}
		if total == 0 {
			continue
		}
		bestLbl, bestCnt := service.PickMajorityLabel(labelCount, sizes)
		purity := float64(bestCnt) / float64(sizes[bestLbl])
		puritySum += purity
		purityN++
		fmt.Printf("  %-10s total=%3d inMajorityCluster=%3d majorityClusterSize=%5d purity=%.3f\n", np.name, total, bestCnt, sizes[bestLbl], purity)
	}
	meanPurity := 0.0
	if purityN > 0 {
		meanPurity = puritySum / float64(purityN)
	}
	fmt.Printf("named-person mean purity=%.4f (n=%d persons)\n", meanPurity, purityN)

	// Residual largest cluster's ClusterConfidence (production formula:
	// avg cosine similarity of members to centroid), vs the MinPersonConfidence
	// gate: does B7's cohesion floor (config.MinPersonConfidence, default 0.5)
	// hide this residual cluster from the People list if it were exposed as
	// an unnamed auto-person?
	var members [][]float32
	for i, l := range labels {
		if l == maxLbl {
			members = append(members, vecs[i])
		}
	}
	centroid := service.ComputeCentroid(members)
	conf := service.ClusterConfidence(members, centroid)
	gated := conf < minConf
	fmt.Printf("residual largest cluster: size=%d ClusterConfidence=%.4f vs MinPersonConfidence=%.2f gate -> %s\n",
		len(members), conf, minConf, map[bool]string{true: "HIDDEN (below gate, would not surface as an unnamed person)", false: "VISIBLE (at/above gate, would surface as an unnamed person)"}[gated])

	// Garbage-cluster retention: how much of the original 2612-face mega
	// cluster survives inside this residual largest cluster.
	garbageSet := map[int]bool{}
	for _, fid := range garbageFaceIDs {
		if gi, ok := idOf[fid]; ok {
			garbageSet[gi] = true
		}
	}
	retained := 0
	for i, l := range labels {
		if l == maxLbl && garbageSet[i] {
			retained++
		}
	}
	fmt.Printf("garbage-cluster retention in residual: %d/%d (%.1f%%)\n", retained, len(garbageSet), 100*float64(retained)/float64(len(garbageSet)))
}

func queryPersonFaceIDs(db *sql.DB, personID string) []string {
	rows, err := db.Query(`SELECT face_id FROM face_person WHERE person_id = ?`, personID)
	if err != nil {
		log.Fatalf("query person faces: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var fid string
		if err := rows.Scan(&fid); err != nil {
			log.Fatalf("scan: %v", err)
		}
		out = append(out, fid)
	}
	return out
}
