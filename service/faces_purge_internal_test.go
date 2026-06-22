package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
)

// purgeEmptyAutoPersons 只清「非锚定且无成员」的孤儿 person:
//   - 命名(锚定)person 保留
//   - 有成员脸的 person 保留
//   - 无成员的非锚定 person 删除
func TestPurgeEmptyAutoPersons(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "fp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mustExec := func(q string, args ...interface{}) {
		if _, e := db.Exec(q, args...); e != nil {
			t.Fatalf("exec %q: %v", q, e)
		}
	}
	mustExec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/x/a1.jpg','indexed')`)
	mustExec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('f1','a1','{}',X'00000000')`)
	mustExec(`INSERT INTO persons(id, name, created_at, updated_at) VALUES('p_named','Bob',0,0)`)   // 锚定:命名
	mustExec(`INSERT INTO persons(id, name, created_at, updated_at) VALUES('p_orphan','',0,0)`)     // 孤儿:非锚定、无成员
	mustExec(`INSERT INTO persons(id, name, created_at, updated_at) VALUES('p_member','',0,0)`)     // 非锚定但有成员
	mustExec(`INSERT INTO face_person(face_id, person_id) VALUES('f1','p_member')`)

	if err := NewFaceService(db).purgeEmptyAutoPersons(context.Background()); err != nil {
		t.Fatal(err)
	}

	count := func(q string) int {
		var n int
		if e := db.QueryRow(q).Scan(&n); e != nil {
			t.Fatal(e)
		}
		return n
	}
	if got := count(`SELECT COUNT(*) FROM persons WHERE id='p_orphan'`); got != 0 {
		t.Fatalf("孤儿 person 应被删除,仍有 %d", got)
	}
	if got := count(`SELECT COUNT(*) FROM persons WHERE id='p_named'`); got != 1 {
		t.Fatalf("命名(锚定)person 应保留")
	}
	if got := count(`SELECT COUNT(*) FROM persons WHERE id='p_member'`); got != 1 {
		t.Fatalf("有成员的 person 应保留")
	}
}

// 少量未分配人脸 + 索引已结束(无 pending)时应触发聚类;仍在索引(有 pending)时不触发。
func TestShouldClusterUnassigned(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "sc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(q string, args ...interface{}) {
		if _, e := db.Exec(q, args...); e != nil {
			t.Fatalf("exec %q: %v", q, e)
		}
	}
	svc := NewFaceService(db)

	// 无人脸 → 不触发
	if svc.shouldClusterUnassigned(context.Background()) {
		t.Fatal("无未分配人脸时不应触发")
	}

	// 1 张已索引资产 + 1 张未分配人脸(< 阈值 50),无 pending → 应触发
	mustExec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/x/a1.jpg','indexed')`)
	mustExec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('f1','a1','{}',X'00000000')`)
	if !svc.shouldClusterUnassigned(context.Background()) {
		t.Fatal("少量未分配人脸且索引结束时应触发聚类")
	}

	// 再加一张 pending 资产(仍在索引)→ 不应触发
	mustExec(`INSERT INTO assets(id, file_path, status) VALUES('a2','/x/a2.jpg','pending')`)
	if svc.shouldClusterUnassigned(context.Background()) {
		t.Fatal("仍有 pending(索引未结束)时不应触发")
	}
}

// 安全网去抖:索引活动还没安静够久时,即使无 pending 也不触发(避免大上传途中空档误触发)。
func TestShouldClusterUnassignedDebounce(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "scd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mustExec := func(q string, args ...interface{}) {
		if _, e := db.Exec(q, args...); e != nil {
			t.Fatalf("exec %q: %v", q, e)
		}
	}
	svc := NewFaceService(db)
	mustExec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/x/a1.jpg','indexed')`)
	mustExec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('f1','a1','{}',X'00000000')`)

	// 索引刚活动过(idle 很短)→ 去抖阻断,不触发
	svc.SetIndexIdleSource(func() time.Duration { return 2 * time.Second })
	if svc.shouldClusterUnassigned(context.Background()) {
		t.Fatal("索引活动未安静够久时不应触发(去抖)")
	}

	// 索引已安静够久 → 允许触发
	svc.SetIndexIdleSource(func() time.Duration { return clusterQuietPeriod + time.Second })
	if !svc.shouldClusterUnassigned(context.Background()) {
		t.Fatal("索引安静够久且无 pending 时应触发")
	}
}
