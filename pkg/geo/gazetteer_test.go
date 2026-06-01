package geo

import (
	"strings"
	"testing"
)

const fxCities = "1850147\tTokyo\t35.6895\t139.6917\tJP\t40\n" +
	"5128581\tNew York\t40.7143\t-74.0060\tUS\tNY\n" +
	"2643743\tLondon\t51.5085\t-0.1257\tGB\tENG\n"

const fxCountries = "JP\tJapan\tAS\nUS\tUnited States\tNA\nGB\tUnited Kingdom\tEU\n"

func testGaz(t *testing.T) *Gazetteer {
	t.Helper()
	g, err := LoadFrom(strings.NewReader(fxCities), strings.NewReader(fxCountries))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	return g
}

func TestReverseGeocodeNearest(t *testing.T) {
	g := testGaz(t)
	r, ok := g.ReverseGeocode(35.659, 139.700)
	if !ok {
		t.Fatal("expected a match")
	}
	if r.CityID != 1850147 || r.City != "Tokyo" || r.Country != "Japan" || r.Region != "asia" {
		t.Fatalf("got %+v", r)
	}
	if r.DistKm > 20 {
		t.Fatalf("dist too large: %.1f", r.DistKm)
	}
}

func TestReverseGeocodePicksClosest(t *testing.T) {
	g := testGaz(t)
	r, _ := g.ReverseGeocode(40.70, -74.01)
	if r.CityID != 5128581 {
		t.Fatalf("expected New York, got %+v", r)
	}
}
