package geo

import (
	"math"
	"testing"
)

func TestHaversineKm(t *testing.T) {
	// Tokyo (35.69,139.69) -> Osaka (34.69,135.50) ≈ 400km
	d := HaversineKm(35.69, 139.69, 34.69, 135.50)
	if math.Abs(d-400) > 30 {
		t.Fatalf("Tokyo->Osaka = %.1fkm, want ~400", d)
	}
	if HaversineKm(10, 20, 10, 20) != 0 {
		t.Fatalf("same point must be 0")
	}
}
