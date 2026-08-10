package service

import (
	"path/filepath"
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/sqlite"
)

// TestSweepSkipsRestoredPerson closes the TOCTOU race between
// PurgeDuePersons' outer SELECT and a concurrent RestorePerson: even when
// the sweep already picked up an id as overdue, purgeDuePerson re-checks
// the guard inside its own transaction and must back off once the person
// has been restored in the meantime.
func TestSweepSkipsRestoredPerson(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "sweep-race.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec(
		`INSERT INTO persons(id, name, hidden, purge_at, created_at, updated_at)
		 VALUES('p1', '', 1, datetime('now', '-1 seconds'), 0, 0)`); err != nil {
		t.Fatal(err)
	}

	svc := NewPersonService(db)

	// Simulate the race: the sweep's outer SELECT already found p1 overdue,
	// but before its purge transaction runs, the user restores it.
	if err := svc.RestorePerson("p1"); err != nil {
		t.Fatal(err)
	}

	// The sweep now runs its guarded per-person purge with the stale id.
	if err := svc.purgeDuePerson("p1"); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM persons WHERE id='p1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("restored person must survive the sweep, got count=%d", n)
	}
}
