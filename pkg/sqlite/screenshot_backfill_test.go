package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBackfillScreenshots verifies that the one-time SQL backfill classifies
// pre-existing assets exactly like service.detectScreenshot: filename markers
// OR PNG-without-camera-EXIF, never videos. This guards the SQL mirror of the
// Go heuristic against drift.
func TestBackfillScreenshots(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "bf.db"))
	require.NoError(t, err)
	defer db.Close()

	// Seed assets (is_screenshot defaults to 0, as a legacy DB would have).
	_, err = db.Exec(`INSERT INTO assets(id, file_path, mime_type, original_name, is_live_photo_video, status) VALUES
		('a-name-en',   '/g/Screenshot_1.png', 'image/png',  'Screenshot_1.png',          0, 'indexed'),
		('a-name-zh',   '/g/jp.jpg',           'image/jpeg', '截屏2024.jpg',               0, 'indexed'),
		('a-png-noexif','/g/x.png',            'image/png',  'x.png',                     0, 'indexed'),
		('a-jpg-cam',   '/g/IMG_1.jpg',        'image/jpeg', 'IMG_1.jpg',                 0, 'indexed'),
		('a-png-cam',   '/g/edit.png',         'image/png',  'edit.png',                  0, 'indexed'),
		('a-jpg-plain', '/g/r.jpg',            'image/jpeg', 'r.jpg',                     0, 'indexed'),
		('a-video',     '/g/Screenshot.mp4',   'video/mp4',  'Screenshot.mp4',            0, 'indexed')`)
	require.NoError(t, err)

	// Camera EXIF for the two assets that must NOT be flagged on the EXIF signal.
	_, err = db.Exec(`INSERT INTO asset_exif(asset_id, make, model, iso, aperture, focal_length) VALUES
		('a-name-zh', 'Apple', 'iPhone 15', 100, 1.8, 6.86),
		('a-jpg-cam', 'Apple', 'iPhone 15', 100, 1.8, 6.86),
		('a-png-cam', 'Canon', '',          0,   0,   0)`)
	require.NoError(t, err)

	require.NoError(t, backfillScreenshots(db))

	want := map[string]int{
		"a-name-en":    1, // filename marker (english)
		"a-name-zh":    1, // filename marker wins despite camera EXIF
		"a-png-noexif": 1, // PNG, no camera EXIF
		"a-jpg-cam":    0, // camera JPEG
		"a-png-cam":    0, // PNG but has Make → not a screenshot
		"a-jpg-plain":  0, // plain JPEG, no marker
		"a-video":      0, // video never qualifies
	}
	for id, exp := range want {
		var got int
		require.NoError(t, db.QueryRow(`SELECT is_screenshot FROM assets WHERE id=?`, id).Scan(&got))
		require.Equalf(t, exp, got, "asset %s", id)
	}
}
