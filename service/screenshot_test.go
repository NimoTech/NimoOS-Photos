package service

import (
	"testing"

	"github.com/NimoTech/NimoOS-Photos/pkg/exif"
)

func TestDetectScreenshot(t *testing.T) {
	cameraExif := &exif.Result{Make: "Apple", Model: "iPhone 15 Pro", ISO: 100, Aperture: 1.8, FocalLength: 6.86}
	emptyExif := &exif.Result{}

	cases := []struct {
		name         string
		originalName string
		mime         string
		ex           *exif.Result
		want         bool
	}{
		// ── Filename markers ──────────────────────────────────────────────
		{"english screenshot prefix", "Screenshot_20240601_101010.png", "image/png", emptyExif, true},
		{"english screen shot space", "Screen Shot 2024-06-01 at 10.10.10.png", "image/png", emptyExif, true},
		{"english screen_shot underscore", "screen_shot.jpg", "image/jpeg", emptyExif, true},
		{"chinese jieping", "截屏2024-06-01.jpg", "image/jpeg", cameraExif, true},
		{"chinese jietu", "我的截图.png", "image/png", cameraExif, true},
		{"chinese pingmukuaizhao", "屏幕快照 2024-06-01.png", "image/png", cameraExif, true},
		{"marker case-insensitive", "MY-SCREENSHOT.PNG", "image/png", cameraExif, true},

		// ── PNG without camera EXIF ───────────────────────────────────────
		{"png no exif", "image123.png", "image/png", emptyExif, true},
		{"png nil exif", "image123.png", "image/png", nil, true},

		// ── Real camera photos (not screenshots) ──────────────────────────
		{"jpeg camera photo", "IMG_0001.jpg", "image/jpeg", cameraExif, false},
		{"png exported from camera (has Make)", "edited.png", "image/png", &exif.Result{Make: "Canon"}, false},
		{"png with iso only", "edited.png", "image/png", &exif.Result{ISO: 200}, false},
		{"jpeg no exif but no marker", "random.jpg", "image/jpeg", emptyExif, false},
		{"webp no marker", "random.webp", "image/webp", emptyExif, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectScreenshot(tc.originalName, tc.mime, tc.ex)
			if got != tc.want {
				t.Errorf("detectScreenshot(%q, %q, %+v) = %v, want %v",
					tc.originalName, tc.mime, tc.ex, got, tc.want)
			}
		})
	}
}
