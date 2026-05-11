package service

import (
	"context"
	"database/sql"
	"math"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	dbscanEpsilon    = 0.6
	dbscanMinPoints  = 1
	clusterBatchSize = 50
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

// FaceService handles face clustering and person management.
type FaceService struct {
	db *sql.DB
}

// NewFaceService creates a new FaceService backed by the given database.
func NewFaceService(db *sql.DB) *FaceService {
	return &FaceService{db: db}
}

// RunClustering reads all face embeddings, runs DBSCAN, and rebuilds the
// persons and face_person tables from scratch.
func (s *FaceService) RunClustering() error {
	// 1. Load all face detections.
	rows, err := s.db.Query(`SELECT id, asset_id, embedding FROM face_detections`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type faceRow struct {
		id      string
		assetID string
		vec     []float32
	}

	var faces []faceRow
	for rows.Next() {
		var f faceRow
		var blob []byte
		if err := rows.Scan(&f.id, &f.assetID, &blob); err != nil {
			return err
		}
		f.vec = sqlite.DeserializeFloat32(blob)
		faces = append(faces, f)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if len(faces) == 0 {
		return nil
	}

	// 2. Build embedding matrix.
	vecs := make([][]float32, len(faces))
	for i, f := range faces {
		vecs[i] = f.vec
	}

	// 3. Run DBSCAN.
	labels := DBSCAN(vecs, dbscanEpsilon, dbscanMinPoints)

	// 4. Rebuild persons and face_person inside a transaction.
	tx, err := s.db.Begin()
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

	// Map cluster label → Person ID.
	labelToPersonID := make(map[int]string)
	// Map cluster label → cover asset ID (first face seen for that cluster).
	labelToCover := make(map[int]string)

	for i, f := range faces {
		label := labels[i]
		if _, exists := labelToPersonID[label]; !exists {
			personID := uuid.NewString()
			labelToPersonID[label] = personID
			labelToCover[label] = f.assetID
			if _, err = tx.Exec(
				`INSERT INTO persons(id, name, cover_asset_id) VALUES(?, '', ?)`,
				personID, f.assetID,
			); err != nil {
				return err
			}
		}
	}

	for i, f := range faces {
		label := labels[i]
		personID := labelToPersonID[label]
		if _, err = tx.Exec(
			`INSERT INTO face_person(face_id, person_id) VALUES(?, ?)`,
			f.id, personID,
		); err != nil {
			return err
		}
	}

	return tx.Commit()
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
					if err := s.RunClustering(); err != nil {
						zap.L().Error("face clustering failed", zap.Error(err))
					}
				}
			}
		}
	}()
}
