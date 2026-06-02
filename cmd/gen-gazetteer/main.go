// Command gen-gazetteer converts GeoNames cities15000.txt + countryInfo.txt
// into the compact gzipped TSV files embedded by pkg/geo.
//
// Usage:
//
//	go run ./cmd/gen-gazetteer -cities cities15000.txt -countries countryInfo.txt -out pkg/geo/data
//
// GeoNames data is licensed CC-BY 4.0 (https://www.geonames.org/).
package main

import (
	"bufio"
	"compress/gzip"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	citiesIn := flag.String("cities", "", "path to GeoNames cities15000.txt")
	countriesIn := flag.String("countries", "", "path to GeoNames countryInfo.txt")
	out := flag.String("out", "pkg/geo/data", "output directory")
	flag.Parse()
	if *citiesIn == "" || *countriesIn == "" {
		fmt.Fprintln(os.Stderr, "both -cities and -countries are required")
		os.Exit(2)
	}
	if err := genCountries(*countriesIn, filepath.Join(*out, "countries.tsv.gz")); err != nil {
		panic(err)
	}
	if err := genCities(*citiesIn, filepath.Join(*out, "cities15000.tsv.gz")); err != nil {
		panic(err)
	}
	fmt.Println("gazetteer generated into", *out)
}

// row is a parsed GeoNames city line we keep around for the two-pass filter.
type row struct {
	id, name, lat, lon, country, admin1, pop, fcode string
	latF, lonF                                      float64
}

// subDivisionCodes are administrative sub-units of a larger city. When one of
// these sits within capitalSwallowKm of a national/SAR capital (PPLC) it is a
// district of that capital (e.g. Macau's parishes "São Lourenço", "Sé", which
// GeoNames even mis-tags under CN) and only fragments the metro, so we drop it.
const capitalSwallowKm = 8.0

func isSubDivision(fcode string) bool {
	switch fcode {
	case "PPLX", "PPLA2", "PPLA3", "PPLA4":
		return true
	default:
		return false
	}
}

func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Asin(math.Sqrt(a))
}

func genCities(in, out string) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()

	// Pass 1: parse every row and remember where the capitals (PPLC) are.
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var rows []row
	type pt struct{ lat, lon float64 }
	var capitals []pt
	for sc.Scan() {
		// GeoNames geoname table columns (tab separated):
		// 0 id, 1 name, 4 lat, 5 lon, 7 feature code, 8 country, 10 admin1, 14 population
		c := strings.Split(sc.Text(), "\t")
		if len(c) < 15 {
			continue
		}
		latF, _ := strconv.ParseFloat(c[4], 64)
		lonF, _ := strconv.ParseFloat(c[5], 64)
		pop := c[14]
		if pop == "" {
			pop = "0"
		}
		rows = append(rows, row{
			id: c[0], name: c[1], lat: c[4], lon: c[5], country: c[8],
			admin1: c[10], pop: pop, fcode: c[7], latF: latF, lonF: lonF,
		})
		if c[7] == "PPLC" {
			capitals = append(capitals, pt{latF, lonF})
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}

	// Pass 2: drop city sub-divisions that a capital swallows, then emit
	// id, name, lat, lon, country, admin1, population (population drives the
	// metro-snap in pkg/geo so a point resolves to its dominant city).
	w, gz, err := openGz(out)
	if err != nil {
		return err
	}
	defer w.Close()
	defer gz.Close()
	for _, r := range rows {
		if isSubDivision(r.fcode) {
			swallowed := false
			for _, cap := range capitals {
				if haversineKm(r.latF, r.lonF, cap.lat, cap.lon) <= capitalSwallowKm {
					swallowed = true
					break
				}
			}
			if swallowed {
				continue
			}
		}
		fmt.Fprintf(gz, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.id, r.name, r.lat, r.lon, r.country, r.admin1, r.pop)
	}
	return nil
}

func genCountries(in, out string) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()
	w, gz, err := openGz(out)
	if err != nil {
		return err
	}
	defer w.Close()
	defer gz.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		// countryInfo.txt columns: 0 ISO, 4 Country, 8 Continent
		c := strings.Split(line, "\t")
		if len(c) < 9 || c[0] == "" {
			continue
		}
		fmt.Fprintf(gz, "%s\t%s\t%s\n", c[0], c[4], c[8])
	}
	return sc.Err()
}

func openGz(path string) (*os.File, *gzip.Writer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, nil, err
	}
	return f, gzip.NewWriter(f), nil
}
