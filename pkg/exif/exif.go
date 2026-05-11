// Package exif provides best-effort EXIF metadata extraction from media files.
// Parse never returns an error; callers receive a zero-value Result when data
// is absent or the file contains no EXIF segment.
package exif

import (
	"io"
	"time"

	goexif "github.com/rwcarlsen/goexif/exif"
	"github.com/rwcarlsen/goexif/mknote"
	"github.com/rwcarlsen/goexif/tiff"
)

func init() {
	// register maker-note parsers (Apple, Nikon, Canon, etc.) so that
	// vendor-specific tags such as ContentIdentifier are decoded.
	goexif.RegisterParsers(mknote.All...)
}

// Result holds the metadata extracted from a single media file.
// Every field is best-effort; the zero value means the field was absent.
type Result struct {
	TakenAt   time.Time
	Latitude  float64
	Longitude float64
	Make      string
	Model     string
	Width     int
	Height    int
	ContentID string // Apple/Google Live Photo UUID
}

// Parse reads EXIF metadata from r. It never returns an error; if the data
// cannot be parsed a zero-value Result is returned instead.
func Parse(r io.Reader) *Result {
	res := &Result{}

	x, err := goexif.Decode(r)
	if err != nil {
		// file has no EXIF segment — return zero result
		return res
	}

	// capture date/time
	if t, err := x.DateTime(); err == nil {
		res.TakenAt = t
	}

	// capture GPS coordinates (both must succeed to be useful)
	if lat, lon, err := x.LatLong(); err == nil {
		res.Latitude = lat
		res.Longitude = lon
	}

	// camera make / model
	if tag, err := x.Get(goexif.Make); err == nil {
		if s, err := tag.StringVal(); err == nil {
			res.Make = s
		}
	}
	if tag, err := x.Get(goexif.Model); err == nil {
		if s, err := tag.StringVal(); err == nil {
			res.Model = s
		}
	}

	// image dimensions
	if tag, err := x.Get(goexif.PixelXDimension); err == nil {
		if n, err := tag.Int(0); err == nil {
			res.Width = n
		}
	}
	if tag, err := x.Get(goexif.PixelYDimension); err == nil {
		if n, err := tag.Int(0); err == nil {
			res.Height = n
		}
	}

	// Apple Live Photo / Google Motion Photo content UUID
	// Walk all fields looking for ContentIdentifier by name so the code
	// is resilient to vendor-specific tag number variations.
	x.Walk(walkerFunc(func(name goexif.FieldName, tag *tiff.Tag) error {
		if name == "ContentIdentifier" || name == "MediaGroupUUID" {
			if s, err := tag.StringVal(); err == nil && s != "" {
				res.ContentID = s
			}
		}
		return nil
	}))

	return res
}

// walkerFunc adapts a closure to the goexif.Walker interface.
type walkerFunc func(goexif.FieldName, *tiff.Tag) error

func (f walkerFunc) Walk(name goexif.FieldName, tag *tiff.Tag) error {
	return f(name, tag)
}
