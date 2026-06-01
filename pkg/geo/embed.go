package geo

import (
	"compress/gzip"
	"embed"
	"fmt"
)

//go:embed data/cities15000.tsv.gz data/countries.tsv.gz
var dataFS embed.FS

// Load builds a Gazetteer from the embedded GeoNames subset.
func Load() (*Gazetteer, error) {
	cf, err := dataFS.Open("data/cities15000.tsv.gz")
	if err != nil {
		return nil, fmt.Errorf("geo.Load cities: %w", err)
	}
	defer cf.Close()
	cgz, err := gzip.NewReader(cf)
	if err != nil {
		return nil, fmt.Errorf("geo.Load cities gz: %w", err)
	}
	defer cgz.Close()

	nf, err := dataFS.Open("data/countries.tsv.gz")
	if err != nil {
		return nil, fmt.Errorf("geo.Load countries: %w", err)
	}
	defer nf.Close()
	ngz, err := gzip.NewReader(nf)
	if err != nil {
		return nil, fmt.Errorf("geo.Load countries gz: %w", err)
	}
	defer ngz.Close()

	return LoadFrom(cgz, ngz)
}
