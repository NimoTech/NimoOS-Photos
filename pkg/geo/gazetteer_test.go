package geo

import (
	"strings"
	"testing"
)

const fxCities = "1850147\tTokyo\t35.6895\t139.6917\tJP\t40\t8336599\n" +
	"5128581\tNew York\t40.7143\t-74.0060\tUS\tNY\t8175133\n" +
	"2643743\tLondon\t51.5085\t-0.1257\tGB\tENG\t7556900\n" +
	// A tiny neighbourhood ~3 km from Tokyo's centre: must snap to Tokyo.
	"9999001\tShibuya\t35.6617\t139.7041\tJP\t40\t221801\n" +
	// An isolated small town far from any metro: must stay itself.
	"9999002\tFarTown\t10.0000\t10.0000\tJP\t40\t18000\n"

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

// A point sitting on a tiny neighbourhood must snap to the dominant metro that
// covers it rather than resolving to the neighbourhood itself.
func TestReverseGeocodeSnapsToMetro(t *testing.T) {
	g := testGaz(t)
	// Right on Shibuya (a 220k-pop neighbourhood ~3 km from Tokyo centre).
	r, ok := g.ReverseGeocode(35.6617, 139.7041)
	if !ok {
		t.Fatal("expected a match")
	}
	if r.CityID != 1850147 || r.City != "Tokyo" {
		t.Fatalf("expected snap to Tokyo, got %+v", r)
	}
}

// An isolated small town with no populous metro nearby must stay itself.
func TestReverseGeocodeKeepsIsolatedTown(t *testing.T) {
	g := testGaz(t)
	r, ok := g.ReverseGeocode(10.001, 10.001)
	if !ok {
		t.Fatal("expected a match")
	}
	if r.CityID != 9999002 || r.City != "FarTown" {
		t.Fatalf("expected isolated FarTown, got %+v", r)
	}
}
