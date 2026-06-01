package service

// Place is one aggregated city in the Places list.
type Place struct {
	Key     int32    `json:"key"`
	Region  string   `json:"region"`
	Country string   `json:"country"`
	City    string   `json:"city"`
	Lon     float64  `json:"lon"`
	Lat     float64  `json:"lat"`
	Count   int      `json:"count"`
	Recent  bool     `json:"recent"`
	Last    string   `json:"last"`
	Trips   int      `json:"trips"`
	Home    bool     `json:"home"`
	Thumbs  []string `json:"thumbs"`
}

// RegionCount is a region id + label + city count for the rail headers.
type RegionCount struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// PlacesStats is the global summary.
type PlacesStats struct {
	Cities    int `json:"cities"`
	Countries int `json:"countries"`
	Photos    int `json:"photos"`
}

// PlacesResponse is the GET /places payload.
type PlacesResponse struct {
	Regions []RegionCount `json:"regions"`
	Places  []Place       `json:"places"`
	Stats   PlacesStats   `json:"stats"`
}

// Spot is a fine-grained cluster within a city.
type Spot struct {
	Key   string  `json:"key"`
	Name  string  `json:"name"`
	Lon   float64 `json:"lon"`
	Lat   float64 `json:"lat"`
	Count int     `json:"count"`
	Thumb string  `json:"thumb"`
}

// Insight is a structured, i18n-renderable observation.
type Insight struct {
	Ico    string                 `json:"ico"`
	Key    string                 `json:"key"`
	Params map[string]interface{} `json:"params"`
}

// Visit is one detected trip to a city.
type Visit struct {
	When    string   `json:"when"`
	From    string   `json:"from"`
	To      string   `json:"to"`
	Current bool     `json:"current"`
	Days    int      `json:"days"`
	Photos  int      `json:"photos"`
	Faces   []string `json:"faces"`
	Spots   int      `json:"spots"`
	Thumbs  []string `json:"thumbs"`
}

// PlaceDetail is the GET /places/{key} payload.
type PlaceDetail struct {
	Place
	CoverAssetID string    `json:"coverAssetId,omitempty"`
	Spots        []Spot    `json:"spots"`
	Insights     []Insight `json:"insights"`
	Visits       []Visit   `json:"visits"`
	Recent       []string  `json:"recent"`
}

var regionLabels = map[string]string{
	"asia": "Asia", "americas": "Americas", "europe": "Europe",
	"africa": "Africa", "oceania": "Oceania", "antarctica": "Antarctica",
}

// CoverCandidatesResult is the GET /places/{key}/cover-candidates payload.
type CoverCandidatesResult struct {
	Tabs       []CoverTab `json:"tabs"`
	Items      []string   `json:"items"`
	Page       int        `json:"page"`
	TotalPages int        `json:"totalPages"`
	Total      int        `json:"total"`
}

// CoverTab is one source tab with its photo count.
type CoverTab struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Icon  string `json:"icon"`
	Count int    `json:"count"`
}
