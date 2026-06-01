package geo

import "testing"

func TestEmbeddedLoad(t *testing.T) {
	g, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if g.Len() < 10000 {
		t.Fatalf("expected >10k cities, got %d", g.Len())
	}
	r, ok := g.ReverseGeocode(35.68, 139.76)
	if !ok || r.Country != "Japan" || r.Region != "asia" {
		t.Fatalf("Tokyo reverse geocode wrong: %+v ok=%v", r, ok)
	}
}
