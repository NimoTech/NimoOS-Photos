package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
)

// purgeEmptyAutoPersons only cleans up orphan persons that are "non-anchored
// and have no members":
//   - named (anchored) persons are kept
//   - persons with member faces are kept
//   - non-anchored persons with no members are deleted
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
	mustExec(`INSERT INTO persons(id, name, created_at, updated_at) VALUES('p_named','Bob',0,0)`) // anchored: named
	mustExec(`INSERT INTO persons(id, name, created_at, updated_at) VALUES('p_orphan','',0,0)`)   // orphan: non-anchored, no members
	mustExec(`INSERT INTO persons(id, name, created_at, updated_at) VALUES('p_member','',0,0)`)   // non-anchored but has members
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
		t.Fatalf("orphan person should have been deleted, still %d left", got)
	}
	if got := count(`SELECT COUNT(*) FROM persons WHERE id='p_named'`); got != 1 {
		t.Fatalf("named (anchored) person should be kept")
	}
	if got := count(`SELECT COUNT(*) FROM persons WHERE id='p_member'`); got != 1 {
		t.Fatalf("person with members should be kept")
	}
}

// Should trigger when there's a small number of unassigned faces and
// indexing has finished (no pending); should not trigger while still
// indexing (has pending).
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

	// No faces -> should not trigger
	if svc.shouldClusterUnassigned(context.Background()) {
		t.Fatal("should not trigger when there are no unassigned faces")
	}

	// 1 indexed asset + 1 unassigned face (< threshold of 50), no pending -> should trigger
	mustExec(`INSERT INTO assets(id, file_path, status) VALUES('a1','/x/a1.jpg','indexed')`)
	mustExec(`INSERT INTO face_detections(id, asset_id, bbox, embedding) VALUES('f1','a1','{}',X'00000000')`)
	if !svc.shouldClusterUnassigned(context.Background()) {
		t.Fatal("should trigger clustering when there's a small number of unassigned faces and indexing has finished")
	}

	// Add one more pending asset (still indexing) -> should not trigger
	mustExec(`INSERT INTO assets(id, file_path, status) VALUES('a2','/x/a2.jpg','pending')`)
	if svc.shouldClusterUnassigned(context.Background()) {
		t.Fatal("should not trigger while there is still pending (indexing not finished)")
	}
}

// Safety-net debounce: when indexing activity hasn't been quiet long enough,
// should not trigger even without pending (avoids a false trigger on a
// momentary gap mid-upload).
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

	// Indexing was just active (idle duration is short) -> debounce blocks it, should not trigger
	svc.SetIndexIdleSource(func() time.Duration { return 2 * time.Second })
	if svc.shouldClusterUnassigned(context.Background()) {
		t.Fatal("should not trigger (debounce) when indexing activity hasn't been quiet long enough")
	}

	// Indexing has been quiet long enough -> allowed to trigger
	svc.SetIndexIdleSource(func() time.Duration { return clusterQuietPeriod + time.Second })
	if !svc.shouldClusterUnassigned(context.Background()) {
		t.Fatal("should trigger when indexing has been quiet long enough and there's no pending")
	}
}
