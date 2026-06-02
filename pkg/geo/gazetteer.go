package geo

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// Gazetteer holds all cities plus a 1°x1° bucket grid for fast nearest lookup.
type Gazetteer struct {
	cities    []City
	countries map[string]Country
	grid      map[int][]int32 // bucket key -> indices into cities
}

func bucketKey(lat, lon float64) int {
	return int(math.Floor(lat+90))*1000 + int(math.Floor(lon+180))
}

// LoadFrom parses cities and countries from TSV readers.
func LoadFrom(cities, countries io.Reader) (*Gazetteer, error) {
	g := &Gazetteer{countries: map[string]Country{}, grid: map[int][]int32{}}

	cs := bufio.NewScanner(countries)
	cs.Buffer(make([]byte, 1024*1024), 1024*1024)
	for cs.Scan() {
		f := strings.Split(cs.Text(), "\t")
		if len(f) < 3 {
			continue
		}
		g.countries[f[0]] = Country{Name: f[1], Continent: f[2]}
	}
	if err := cs.Err(); err != nil {
		return nil, fmt.Errorf("geo: read countries: %w", err)
	}

	sc := bufio.NewScanner(cities)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) < 6 {
			continue
		}
		id, _ := strconv.Atoi(f[0])
		lat, _ := strconv.ParseFloat(f[2], 64)
		lon, _ := strconv.ParseFloat(f[3], 64)
		var pop int64
		if len(f) >= 7 {
			pop, _ = strconv.ParseInt(f[6], 10, 64)
		}
		idx := int32(len(g.cities))
		g.cities = append(g.cities, City{
			ID: int32(id), Name: f[1], Lat: lat, Lon: lon, ISO2: f[4], Admin1: f[5], Population: pop,
		})
		k := bucketKey(lat, lon)
		g.grid[k] = append(g.grid[k], idx)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("geo: read cities: %w", err)
	}
	if len(g.cities) == 0 {
		return nil, fmt.Errorf("geo: empty gazetteer")
	}
	return g, nil
}

// nearest returns the index of the closest city and the distance in km.
func (g *Gazetteer) nearest(lat, lon float64) (int32, float64) {
	best := int32(-1)
	bestD := math.MaxFloat64
	consider := func(idx int32) {
		c := g.cities[idx]
		d := HaversineKm(lat, lon, c.Lat, c.Lon)
		if d < bestD {
			bestD = d
			best = idx
		}
	}
	bLat := int(math.Floor(lat + 90))
	bLon := int(math.Floor(lon + 180))
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			for _, idx := range g.grid[(bLat+dy)*1000+(bLon+dx)] {
				consider(idx)
			}
		}
	}
	if best == -1 {
		for idx := range g.cities {
			consider(int32(idx))
		}
	}
	return best, bestD
}

// metroRadiusKm returns how far a city of the given population "pulls" nearby
// points onto itself. Larger cities dominate a wider area so that the many
// constituent towns of a metropolis (e.g. Hong Kong's districts) all resolve to
// the one metro instead of fragmenting into neighbourhood-sized places.
func metroRadiusKm(pop int64) float64 {
	switch {
	case pop >= 5_000_000:
		return 40
	case pop >= 1_000_000:
		return 30
	case pop >= 500_000:
		return 22
	case pop >= 200_000:
		return 15
	case pop >= 100_000:
		return 10
	default:
		return 0 // only ever chosen by plain nearest-city fallback
	}
}

// metroSnap returns the index of the most populous city (within the given
// country) whose metro radius covers (lat, lon), or -1 if none qualifies. This
// pulls a point onto the dominant nearby city rather than the literally-closest
// hamlet. Candidates are restricted to `iso2` so that, e.g., a Hong Kong point
// snaps to Hong Kong rather than to larger Shenzhen just across the border.
func (g *Gazetteer) metroSnap(lat, lon float64, iso2 string) int32 {
	best := int32(-1)
	var bestPop int64 = -1
	bLat := int(math.Floor(lat + 90))
	bLon := int(math.Floor(lon + 180))
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			for _, idx := range g.grid[(bLat+dy)*1000+(bLon+dx)] {
				c := g.cities[idx]
				if c.ISO2 != iso2 {
					continue
				}
				r := metroRadiusKm(c.Population)
				if r <= 0 || c.Population <= bestPop {
					continue
				}
				if HaversineKm(lat, lon, c.Lat, c.Lon) <= r {
					bestPop = c.Population
					best = idx
				}
			}
		}
	}
	return best
}

// ReverseGeocode returns the dominant nearby city for a coordinate, falling back
// to the literally-closest city when no populous metro covers the point.
func (g *Gazetteer) ReverseGeocode(lat, lon float64) (Resolved, bool) {
	idx, d := g.nearest(lat, lon)
	if idx < 0 {
		return Resolved{}, false
	}
	if snap := g.metroSnap(lat, lon, g.cities[idx].ISO2); snap >= 0 && snap != idx {
		idx = snap
		d = HaversineKm(lat, lon, g.cities[snap].Lat, g.cities[snap].Lon)
	}
	c := g.cities[idx]
	ctry := g.countries[c.ISO2]
	return Resolved{
		CityID:  c.ID,
		City:    c.Name,
		Country: ctry.Name,
		Region:  RegionForContinent(ctry.Continent),
		Admin1:  c.Admin1,
		DistKm:  d,
	}, true
}

// CityByID returns a city by its geonameid.
func (g *Gazetteer) CityByID(id int32) (City, bool) {
	for _, c := range g.cities {
		if c.ID == id {
			return c, true
		}
	}
	return City{}, false
}

// Len returns the number of loaded cities.
func (g *Gazetteer) Len() int { return len(g.cities) }

// NearestFeature returns the closest city name within maxKm, else ("", false).
func (g *Gazetteer) NearestFeature(lat, lon, maxKm float64) (string, bool) {
	idx, d := g.nearest(lat, lon)
	if idx < 0 || d > maxKm {
		return "", false
	}
	return g.cities[idx].Name, true
}
