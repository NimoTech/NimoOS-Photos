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
		idx := int32(len(g.cities))
		g.cities = append(g.cities, City{
			ID: int32(id), Name: f[1], Lat: lat, Lon: lon, ISO2: f[4], Admin1: f[5],
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

// ReverseGeocode returns the nearest city for a coordinate.
func (g *Gazetteer) ReverseGeocode(lat, lon float64) (Resolved, bool) {
	idx, d := g.nearest(lat, lon)
	if idx < 0 {
		return Resolved{}, false
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
