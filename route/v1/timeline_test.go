package v1

import "testing"

// TestValidBucketYM pins the bucket-key validity rules Bucket's 400 check
// relies on: either both year and month are zero (the "unknown date"
// bucket — taken_at and indexed_at both NULL) or both are set with month in
// 1..12. A "half-zero" pair (year set, month zero, or vice versa) can never
// correspond to a real bucket and must be rejected.
func TestValidBucketYM(t *testing.T) {
	cases := []struct {
		name        string
		year, month int
		want        bool
	}{
		{"unknown bucket both zero", 0, 0, true},
		{"normal bucket", 2020, 6, true},
		{"half-zero year set month zero", 2020, 0, false},
		{"half-zero year zero month set", 0, 6, false},
		{"negative year", -1, 6, false},
		{"negative month", 2020, -1, false},
		{"month too large", 2020, 13, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validBucketYM(tc.year, tc.month); got != tc.want {
				t.Fatalf("validBucketYM(%d, %d) = %v, want %v", tc.year, tc.month, got, tc.want)
			}
		})
	}
}
