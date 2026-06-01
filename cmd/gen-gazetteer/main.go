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
	"os"
	"path/filepath"
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

func genCities(in, out string) error {
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
		// GeoNames geoname table columns (tab separated):
		// 0 id, 1 name, 4 lat, 5 lon, 8 country, 10 admin1
		c := strings.Split(sc.Text(), "\t")
		if len(c) < 11 {
			continue
		}
		fmt.Fprintf(gz, "%s\t%s\t%s\t%s\t%s\t%s\n", c[0], c[1], c[4], c[5], c[8], c[10])
	}
	return sc.Err()
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
