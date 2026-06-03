package service

import (
	"strings"

	"github.com/NimoTech/NimoOS-Photos/pkg/exif"
)

// screenshotNameMarkers are case-insensitive substrings whose presence in an
// asset's original filename identifies it as a screenshot. When changing this
// list, mirror it in the SQL backfill in pkg/sqlite/db.go (backfillScreenshots).
var screenshotNameMarkers = []string{
	"screenshot",
	"screen shot",
	"screen_shot",
	"截屏",
	"截图",
	"屏幕快照",
}

// detectScreenshot reports whether a non-video image asset is a screenshot.
// Two independent signals are combined; either one is sufficient:
//
//  1. the filename contains a known screenshot marker, or
//  2. the file is a PNG carrying no camera EXIF (no Make/Model and no
//     ISO/Aperture/FocalLength) — typical of OS screen captures.
//
// Callers pass only images here; videos (including screen recordings) are never
// classified as screenshots. The same heuristic is duplicated as SQL in
// pkg/sqlite/db.go for the one-time backfill of pre-existing rows — keep both
// in sync.
func detectScreenshot(originalName, mime string, ex *exif.Result) bool {
	lowerName := strings.ToLower(originalName)
	for _, m := range screenshotNameMarkers {
		if strings.Contains(lowerName, m) {
			return true
		}
	}
	if strings.EqualFold(mime, "image/png") && !hasCameraExif(ex) {
		return true
	}
	return false
}

// hasCameraExif reports whether ex carries any signal that the image was
// produced by a camera (vendor/model tags or capture parameters).
func hasCameraExif(ex *exif.Result) bool {
	if ex == nil {
		return false
	}
	return ex.Make != "" || ex.Model != "" || ex.ISO != 0 || ex.Aperture != 0 || ex.FocalLength != 0
}
