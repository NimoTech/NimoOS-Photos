package service_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// person_id lookups on face_person must be index-backed, not full scans.
func TestFacePersonPersonIDIndexed(t *testing.T) {
	db := makeTestFaceDB(t)
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT face_id FROM face_person WHERE person_id = 'x'`)
	require.NoError(t, err)
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		plan.WriteString(detail)
	}
	require.Contains(t, plan.String(), "idx_face_person_person",
		"person_id lookup must use the covering index")
	require.NotContains(t, plan.String(), "SCAN face_person",
		"must not full-scan face_person")
}
