package service

import (
	"os"
	"path/filepath"
	"time"
)

// PruneStaging removes files in stagingDir older than maxAge.
// Returns the count of files removed and any error encountered.
// Missing dir is not an error.
func PruneStaging(stagingDir string, maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			path := filepath.Join(stagingDir, e.Name())
			if err := os.Remove(path); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}
