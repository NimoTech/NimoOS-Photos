// Package geo provides an embedded offline gazetteer (GeoNames cities15000
// subset) and reverse geocoding used by the Photos Places feature.
package geo

// City is one gazetteer entry.
type City struct {
	ID         int32   `json:"id"`   // GeoNames geonameid; used as place_key
	Name       string  `json:"name"` // city name
	Lat        float64 `json:"lat"`
	Lon        float64 `json:"lon"`
	ISO2       string  `json:"iso2"`       // country ISO-3166 alpha-2
	Admin1     string  `json:"admin1"`     // state/province code, used for spot naming fallback
	Population int64   `json:"population"` // GeoNames population; drives metro-snapping
}

// Country maps an ISO2 code to its display name and continent.
type Country struct {
	Name      string
	Continent string // AF/AS/EU/NA/SA/OC/AN
}

// Resolved is the result of reverse geocoding a coordinate.
type Resolved struct {
	CityID  int32
	City    string
	Country string // display name
	Region  string // asia/americas/europe/africa/oceania/antarctica
	Admin1  string
	DistKm  float64
}

// RegionForContinent maps a GeoNames continent code to the 6 UI region ids.
// Returns "" for unknown codes.
func RegionForContinent(code string) string {
	switch code {
	case "AS":
		return "asia"
	case "EU":
		return "europe"
	case "OC":
		return "oceania"
	case "AF":
		return "africa"
	case "NA", "SA":
		return "americas"
	case "AN":
		return "antarctica"
	default:
		return ""
	}
}
