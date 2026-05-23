package v1

import (
	"fmt"
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
			_, err := validateMetadataWithQuota(info, func() (uint64, error) { return 1 << 40, nil })
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
	_, err := validateMetadataWithQuota(info, func() (uint64, error) { return 1 << 40, nil })
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

func TestCheckQuota_Sufficient(t *testing.T) {
	err := checkQuota(1000, func() (uint64, error) { return 1_000_000, nil })
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestCheckQuota_Insufficient(t *testing.T) {
	// uploadLength * 1.05 > available
	err := checkQuota(1000, func() (uint64, error) { return 1000, nil })
	if err == nil {
		t.Fatal("expected quota error, got nil")
	}
}

func TestCheckQuota_StatFails(t *testing.T) {
	err := checkQuota(1000, func() (uint64, error) {
		return 0, fmt.Errorf("statfs failed")
	})
	if err == nil {
		t.Fatal("expected error when statfs fails")
	}
}
