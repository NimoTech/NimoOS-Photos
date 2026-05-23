package v1

import (
	"testing"

	"github.com/tus/tusd/v2/pkg/handler"
)

func TestValidateMetadata(t *testing.T) {
	cases := []struct {
		name    string
		meta    map[string]string
		wantErr bool
	}{
		{"empty filename", map[string]string{"filename": ""}, true},
		{"missing filename", map[string]string{}, true},
		{"path traversal", map[string]string{"filename": "../etc/passwd"}, true},
		{"slash in name", map[string]string{"filename": "a/b.jpg"}, true},
		{"normal filename", map[string]string{"filename": "IMG_3421.HEIC"}, false},
		{"unicode filename", map[string]string{"filename": "照片_2026.jpg"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := handler.HookEvent{
				Upload: handler.FileInfo{MetaData: tc.meta, Size: 1000},
			}
			_, err := validateMetadata(info)
			if (err != nil) != tc.wantErr {
				t.Fatalf("wantErr=%v got err=%v", tc.wantErr, err)
			}
		})
	}
}

func TestValidateMetadata_SizeLimit(t *testing.T) {
	info := handler.HookEvent{
		Upload: handler.FileInfo{
			MetaData: map[string]string{"filename": "x.jpg"},
			Size:     21 * 1024 * 1024 * 1024, // 21 GB
		},
	}
	_, err := validateMetadata(info)
	if err == nil {
		t.Fatal("expected error for oversized upload, got nil")
	}
}

func TestValidateMetadata_ZeroSize(t *testing.T) {
	info := handler.HookEvent{
		Upload: handler.FileInfo{
			MetaData: map[string]string{"filename": "x.jpg"},
			Size:     0,
		},
	}
	_, err := validateMetadata(info)
	if err == nil {
		t.Fatal("expected error for zero-size upload, got nil")
	}
}
