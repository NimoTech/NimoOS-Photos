package service

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	dbscanEpsilon    = 0.6
	dbscanMinPoints  = 1
	clusterBatchSize = 50
	// assignEpsilon 是游离脸吸附到锚定 person 质心的最大余弦距离。
	assignEpsilon = 0.55
	// suggestEpsilon 是「合并建议」配对的余弦距离上界（下界为 dbscanEpsilon）。
	suggestEpsilon = 0.75
)

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

// ComputeCentroid 返回向量集合的逐维平均（质心）。
// 空集合或维度不一致返回 nil。调用方需保证所有向量等长。
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

// ClusterConfidence 返回簇内聚合度 [0,1]：成员到质心平均余弦相似度。
// 单元素簇返回 1.0；空簇返回 0.0。
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

// DBSCANWithProgress 行为同 DBSCAN，但每跨 1% 进度调一次 onProgress。
// onProgress 必非 nil；终止前保证最后一次回调 done==n。
func DBSCANWithProgress(vecs [][]float32, epsilon float64, minPoints int, onProgress func(done, n int)) []int {
	n := len(vecs)
	labels := make([]int, n)
	for i := range labels {
		labels[i] = -1
	}
	visited := make([]bool, n)
	clusterID := 0
	lastReport := -1
	bucket := func(i int) int {
		if n == 0 {
			return 0
		}
		return (i * 100) / n
	}

	for i := 0; i < n; i++ {
		if b := bucket(i); b != lastReport {
			onProgress(i, n)
			lastReport = b
		}
		if visited[i] {
			continue
		}
		visited[i] = true
		neighbors := regionQuery(vecs, i, epsilon)
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
	onProgress(n, n) // 保证终态 done==n
	return labels
}

// faceRow holds a single face detection record loaded from the database.
type faceRow struct {
	id      string
	assetID string
	vec     []float32
}

// FaceService handles face clustering and person management.
type FaceService struct {
	db      *sql.DB
	reg     *TaskRegistry
	running atomic.Bool

	// 失败 backoff：RunClustering 出错后短期内不再触发，避免每分钟重试风暴。
	failMu   sync.Mutex
	nextAttempt time.Time
}

// clusterFailBackoff 是 RunClustering 失败后的最短再次尝试间隔。
const clusterFailBackoff = 30 * time.Minute

// NewFaceService creates a new FaceService backed by the given database.
func NewFaceService(db *sql.DB) *FaceService {
	return &FaceService{db: db}
}

// SetTaskRegistry injects a TaskRegistry so RunClustering can report progress.
func (s *FaceService) SetTaskRegistry(reg *TaskRegistry) { s.reg = reg }

// RunClustering reads all face embeddings, runs DBSCAN, and rebuilds the
// persons and face_person tables from scratch.
// Concurrent calls are safe: the second call returns nil immediately (CAS guard).
func (s *FaceService) RunClustering(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return nil
	}
	defer s.running.Store(false)

	var total int64
	// 与 loadFacesWithProgress 一致：只算关联到现存 asset 的 face_detections。
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM face_detections fd
		JOIN assets a ON a.id = fd.asset_id`).Scan(&total); err != nil {
		return err
	}
	if total == 0 {
		return nil
	}

	taskID := fmt.Sprintf("face_%d", time.Now().UnixNano())
	started := time.Now()
	pub := func(progress float64, status string, errMsg string) {
		if s.reg == nil {
			return
		}
		t := Task{
			ID:        taskID,
			Type:      "face",
			Label:     "识别人物",
			Progress:  progress,
			Status:    status,
			Error:     errMsg,
			StartedAt: started,
		}
		// 终态填入 current/total，前端 done 时拼 "已识别 N 个人脸" toast 文案需要。
		// running 中间态不填，避免节流 publish 把 0 错带到前端造成数字闪。
		if status == "done" || status == "error" {
			t.Current = total
			t.Total = total
		}
		s.reg.Upsert(t)
	}
	pub(0, "running", "")

	faces, err := s.loadFacesWithProgress(ctx, total, func(loaded int64) {
		pub(0.10*float64(loaded)/float64(total), "running", "")
	})
	if err != nil {
		pub(0, "error", fmt.Sprintf("人脸聚类失败：%s", err.Error()))
		return err
	}

	vecs := make([][]float32, len(faces))
	for i, f := range faces {
		vecs[i] = f.vec
	}
	labels := DBSCANWithProgress(vecs, dbscanEpsilon, dbscanMinPoints,
		func(done, n int) {
			if n == 0 {
				return
			}
			pub(0.10+0.75*float64(done)/float64(n), "running", "")
		})

	if err := s.rebuildPersonsWithProgress(ctx, faces, labels,
		func(done, n int) {
			if n == 0 {
				return
			}
			pub(0.85+0.15*float64(done)/float64(n), "running", "")
		},
	); err != nil {
		pub(0, "error", fmt.Sprintf("人脸聚类失败：%s", err.Error()))
		return err
	}

	pub(1, "done", "")
	go func() {
		time.Sleep(taskCleanupDelay)
		if s.reg != nil {
			s.reg.Remove(taskID)
		}
	}()
	return nil
}

func (s *FaceService) loadFacesWithProgress(ctx context.Context, total int64,
	onProgress func(int64),
) ([]faceRow, error) {
	// JOIN assets 过滤孤儿 face_detections（asset_id 指向已删除 asset 的行）。
	// 不 JOIN 的话，rebuildPersons 拿着孤儿的 asset_id 做 cover 会触发
	// persons.cover_asset_id REFERENCES assets(id) 的外键违反。
	rows, err := s.db.QueryContext(ctx, `
		SELECT fd.id, fd.asset_id, fd.embedding
		FROM face_detections fd
		JOIN assets a ON a.id = fd.asset_id`)
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
		if err := rows.Scan(&f.id, &f.assetID, &blob); err != nil {
			return nil, err
		}
		f.vec = sqlite.DeserializeFloat32(blob)
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

func (s *FaceService) rebuildPersonsWithProgress(ctx context.Context, faces []faceRow, labels []int,
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

	if _, err = tx.Exec(`DELETE FROM face_person`); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM persons`); err != nil {
		return err
	}

	labelToPersonID := map[int]string{}
	for i, f := range faces {
		l := labels[i]
		if _, ok := labelToPersonID[l]; !ok {
			personID := uuid.NewString()
			labelToPersonID[l] = personID
			if _, err = tx.Exec(`INSERT INTO persons(id, name, cover_asset_id) VALUES(?, '', ?)`,
				personID, f.assetID); err != nil {
				return err
			}
		}
	}

	n := len(faces)
	lastReport := -1
	for i, f := range faces {
		if _, err = tx.Exec(`INSERT INTO face_person(face_id, person_id) VALUES(?, ?)`,
			f.id, labelToPersonID[labels[i]]); err != nil {
			return err
		}
		if n > 0 {
			if b := (i + 1) * 100 / n; b != lastReport {
				onProgress(i+1, n)
				lastReport = b
			}
		}
	}
	onProgress(n, n)
	err = tx.Commit()
	return err
}

// StartScheduler runs a background goroutine that triggers RunClustering:
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
				// 失败 backoff：上次失败后短期内不再尝试。
				s.failMu.Lock()
				nextOK := s.nextAttempt
				s.failMu.Unlock()
				if !nextOK.IsZero() && t.Before(nextOK) {
					continue
				}

				shouldRun := false

				if t.Hour() == 3 && t.Minute() < 5 {
					shouldRun = true
				} else {
					// Count faces not yet associated with a person.
					var unassigned int
					err := s.db.QueryRowContext(ctx,
						`SELECT COUNT(*) FROM face_detections fd
						 WHERE NOT EXISTS (
							SELECT 1 FROM face_person fp WHERE fp.face_id = fd.id
						 )`,
					).Scan(&unassigned)
					if err == nil && unassigned >= clusterBatchSize {
						shouldRun = true
					}
				}

				if shouldRun {
					if err := s.RunClustering(ctx); err != nil {
						zap.L().Error("face clustering failed", zap.Error(err))
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
