package service

import (
	"context"
	"database/sql"
	"fmt"
	"math"
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
	failMu      sync.Mutex
	nextAttempt time.Time

	// indexIdleFor 返回「距上次索引活动多久」。安全网触发(scheduler)据此去抖,
	// 避免大上传途中索引队列空档(pending==0 瞬间)被误判为上传结束而提前聚类。
	// 为 nil 时(测试 / 未接入)视为始终空闲。
	indexIdleFor func() time.Duration
}

// clusterFailBackoff 是 RunClustering 失败后的最短再次尝试间隔。
const clusterFailBackoff = 30 * time.Minute

// clusterQuietPeriod 是安全网触发前要求的「索引活动安静时长」:索引活动停止超过这段
// 时间才认为整批上传/索引真正结束。需大于活动中典型的逐张处理间隔(ML 每张约数秒)。
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
		JOIN assets a ON a.id = fd.asset_id
		WHERE fd.excluded = 0`).Scan(&total); err != nil {
		return err
	}
	if total == 0 {
		// 没有人脸了(例如照片被全部删除):自动聚类产生的非锚定 person 已无任何成员,
		// 必须在这里清掉,否则人物表会残留孤儿——清理 persons 的唯一路径
		// rebuildPersonsWithProgress 正好被这个早退跳过了。
		return s.purgeAutoPersons(ctx)
	}

	// 本次「新增人脸」= 聚类前仍未分配到任何 person 的人脸数(即新上传照片里新检测到、
	// 尚未聚类的人脸)。供前端「有新增才弹提示、且显示新增数而非总数」。
	// 没有新增(如上传的是无人脸的文档/OCR 图)时为 0,前端据此不弹人脸提示。
	var newFaces int64
	_ = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM face_detections fd
		JOIN assets a ON a.id = fd.asset_id
		WHERE fd.excluded = 0
		  AND NOT EXISTS (SELECT 1 FROM face_person fp WHERE fp.face_id = fd.id)`).Scan(&newFaces)

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
		// 终态填入 current/total（总人脸数）与 added（本次新增数）。
		// running 中间态不填，避免节流 publish 把 0 错带到前端造成数字闪。
		if status == "done" || status == "error" {
			t.Current = total
			t.Total = total
			t.Added = newFaces
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

// purgeAutoPersons 删除所有自动聚类产生的(非锚定:无名字/收藏/关系/隐藏)person
// 及其 face_person 行。锚定人物(用户命名/收藏/关系/隐藏)保留。
// 用于 0 人脸场景下清理孤儿人物;删除顺序与 rebuildPersonsWithProgress 一致。
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

// purgeEmptyAutoPersons 删除「非锚定(无名字/收藏/关系/隐藏)且已无任何成员脸」的 person。
//
// 用于删照片后的孤儿自愈:删 asset 会经 FK 级联删掉 face_detections 和 face_person,
// 但 persons 不在级联链上,会遗留空壳孤儿。而清 persons 的常规路径(rebuildPersons)
// 只在 RunClustering 跑起来时才执行,删除本身不触发聚类,故需独立的周期性自愈。
//
// 安全性:正常聚类后的非锚定 person 都在同一事务里带着 face_person 成员,EXISTS 子查询
// 会判定其非空而保留;此处只清理「已无成员」的空壳,可在任意时刻反复调用。
// shouldClusterUnassigned 判断是否应触发一次聚类(不含每日 03:xx 定时那条):
//   - 未分配人脸 > 0 且已无 pending 资产(索引彻底结束)时,才聚类一次。
//
// 设计:聚类只在「索引彻底安静」后跑一次,不在索引进行中提前触发。
// 早先有「未分配人脸 ≥ clusterBatchSize 就立即聚类」的提前触发,会导致一次上传里
// 索引途中先聚一次、settle 后又聚一次(双跑:用户会看到两条「识别人物」),故移除。
// 代价:超大上传也要等索引全部结束才聚类一次(完成提示稍晚几秒),换来「合并成单条」。
//
// 依赖 pending==0 而非上传批次回调(SetOnBatchDone 基于 ingestTracker 的 current>=total,
// 声明总数>实际处理数时永不归零),否则少量人脸会一直不聚类、persons 始终为 0、
// 「识别人物」任务从不出现。
func (s *FaceService) shouldClusterUnassigned(ctx context.Context) bool {
	// 去抖:索引活动还没安静够久(整批上传/索引可能仍在进行,只是出现了瞬时空档)→ 不触发。
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

func (s *FaceService) loadFacesWithProgress(ctx context.Context, total int64,
	onProgress func(int64),
) ([]faceRow, error) {
	// JOIN assets 过滤孤儿 face_detections（asset_id 指向已删除 asset 的行）。
	// 不 JOIN 的话，rebuildPersons 拿着孤儿的 asset_id 做 cover 会触发
	// persons.cover_asset_id REFERENCES assets(id) 的外键违反。
	//
	// 有意不过滤 offline=1：人脸向量数据本来就完整保留在磁盘上（不依赖原图文件),
	// 聚类结果与 offline 状态无关。若在这里排除 offline 资产，插回移动盘后
	// (MountGuard 标回 offline=0) 会触发一轮重新聚类，导致 person 分组抖动
	// （同一批脸先被踢出聚类、插回后又被重新分配，可能落到不同的 person）。
	// 展示层的过滤已经在 persons.go / search.go 等查询里通过 offline=0 完成，
	// 聚类引擎本身保持"数据在就参与聚类"的稳定语义。
	rows, err := s.db.QueryContext(ctx, `
		SELECT fd.id, fd.asset_id, fd.embedding
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

	// 1. 载入锚定 person（有名字/收藏/关系/隐藏）及其当前成员脸，算质心。
	type anchor struct {
		id       string
		centroid []float32
	}
	anchorRows, err := tx.QueryContext(ctx,
		`SELECT id FROM persons WHERE name!='' OR favorite=1 OR relation!='' OR hidden=1`)
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

	anchored := map[string]bool{} // 锚定 person 名下的 face_id 集合
	anchors := make([]anchor, 0, len(anchorIDs))
	for _, pid := range anchorIDs {
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
		// 记录锚定成员
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
	}

	// 2. 删除自动 person（非锚定）及其 face_person 行。
	if _, err = tx.Exec(`
		DELETE FROM face_person
		WHERE person_id IN (SELECT id FROM persons WHERE NOT (name!='' OR favorite=1 OR relation!='' OR hidden=1))`); err != nil {
		return err
	}
	if _, err = tx.Exec(`
		DELETE FROM persons
		WHERE NOT (name!='' OR favorite=1 OR relation!='' OR hidden=1)`); err != nil {
		return err
	}

	// 3. 游离脸 = 不在锚定成员集合内的脸；先尝试吸附到最近锚定质心。
	// 预编译 face_person INSERT，步骤 3 和 4 共用。
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

	// 4. 剩余游离脸按 DBSCAN label 聚成新自动 person。
	// cover_asset_id / cover_face_id 由 recomputePersonStatsTx 统一设置，这里 INSERT 时不填。
	personStmt, err := tx.PrepareContext(ctx, `INSERT INTO persons(id, name, created_at, updated_at) VALUES(?, '', ?, ?)`)
	if err != nil {
		return err
	}
	defer personStmt.Close()

	labelToPersonID := map[int]string{}
	now := time.Now()
	for _, ff := range free {
		l := labels[ff.idx]
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
	}

	// 5. 为所有 person（含 hidden）回写 centroid/confidence/cover_face_id：
	//    隐藏 person 也需要保持最新质心，否则下次聚类的吸附阶段会用陈旧质心。
	if err = s.recomputePersonStatsTx(ctx, tx); err != nil {
		return err
	}

	n := len(faces)
	onProgress(n, n)
	err = tx.Commit()
	return err
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
		fr, ferr := tx.QueryContext(ctx, `
			SELECT fd.id, fd.asset_id, fd.embedding
			FROM face_person fp
			JOIN face_detections fd ON fd.id = fp.face_id
			WHERE fp.person_id = ? AND fd.excluded = 0`, meta.id)
		if ferr != nil {
			return ferr
		}
		var faceIDs, assetIDs []string
		var vecs [][]float32
		for fr.Next() {
			var fid, aid string
			var blob []byte
			if err = fr.Scan(&fid, &aid, &blob); err != nil {
				fr.Close()
				return err
			}
			faceIDs = append(faceIDs, fid)
			assetIDs = append(assetIDs, aid)
			vecs = append(vecs, sqlite.DeserializeFloat32(blob))
		}
		if cerr := fr.Err(); cerr != nil {
			fr.Close()
			return cerr
		}
		fr.Close()

		if len(vecs) == 0 {
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

		// Select the face nearest to the centroid as cover.
		bestIdx, bestDist := 0, math.MaxFloat64
		for i, v := range vecs {
			if d := cosDist(v, centroid); d < bestDist {
				bestDist = d
				bestIdx = i
			}
		}
		if _, err = coverStmt.ExecContext(ctx,
			sqlite.SerializeFloat32(centroid), conf, faceIDs[bestIdx], assetIDs[bestIdx], now, meta.id); err != nil {
			return err
		}
	}
	return nil
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
				// 孤儿人物自愈(独立于聚类节流/开关):删照片会级联删脸但不删 persons,
				// 这里每分钟清理已无成员的非锚定 person,覆盖所有删除路径。
				if err := s.purgeEmptyAutoPersons(ctx); err != nil {
					zap.L().Warn("purge empty auto-persons failed", zap.Error(err))
				}

				// 失败 backoff：上次失败后短期内不再尝试。
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
