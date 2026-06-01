package geo

import "testing"

func TestRegionForContinent(t *testing.T) {
	cases := map[string]string{
		"AS": "asia", "EU": "europe", "OC": "oceania",
		"AF": "africa", "NA": "americas", "SA": "americas",
		"AN": "antarctica", "??": "",
	}
	for code, want := range cases {
		if got := RegionForContinent(code); got != want {
			t.Fatalf("RegionForContinent(%q)=%q want %q", code, got, want)
		}
	}
}
