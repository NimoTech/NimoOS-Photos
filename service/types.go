package service

import "time"

type Asset struct {
	ID               string     `json:"id"`
	FilePath         string     `json:"filePath"`
	FileSize         int64      `json:"fileSize"`
	MimeType         string     `json:"mimeType"`
	OriginalName     string     `json:"originalName"`
	TakenAt          *time.Time `json:"takenAt,omitempty"`
	DurationMs       int64      `json:"durationMs,omitempty"`
	LivePhotoVideoID string     `json:"livePhotoVideoId,omitempty"`
	IsLivePhotoVideo bool       `json:"isLivePhotoVideo"`
	// HasOCR reports whether OCR recognized any text in this asset (a row in
	// asset_ocr with non-empty text). Replaces the old screenshot heuristic as
	// the third media category: Photos / OCR / Videos.
	HasOCR    bool       `json:"hasOcr"`
	IndexedAt *time.Time `json:"indexedAt,omitempty"`
	Status    string     `json:"status"`
	Checksum  string     `json:"checksum,omitempty"`

	// Joined from asset_exif (populated by GetAsset; absent in ListAssets/SmartSearch).
	Width        int     `json:"width,omitempty"`
	Height       int     `json:"height,omitempty"`
	Latitude     float64 `json:"latitude,omitempty"`
	Longitude    float64 `json:"longitude,omitempty"`
	Make         string  `json:"make,omitempty"`
	Model        string  `json:"model,omitempty"`
	ISO          int     `json:"iso,omitempty"`
	ShutterSpeed string  `json:"shutterSpeed,omitempty"`
	Aperture     float64 `json:"aperture,omitempty"`
	FocalLength  float64 `json:"focalLength,omitempty"`
	Orientation  int     `json:"orientation,omitempty"`
	VideoCodec   string  `json:"videoCodec,omitempty"`
	AudioCodec   string  `json:"audioCodec,omitempty"`
	FrameRate    float64 `json:"frameRate,omitempty"`
	BitRate      int64   `json:"bitRate,omitempty"`
	Rotation     int     `json:"rotation,omitempty"`

	// PlaceName is the human-readable location ("City" or "City, Country") derived
	// from asset_geo. Populated by Timeline/ListAssets/SmartSearch/favorites via
	// enrichPlaceNames so the client shows and filters by city, not just country.
	PlaceName string `json:"placeName,omitempty"`

	// Joined from asset_favorites (populated by List/Timeline/GetAsset when caller
	// supplies a user_id; nil = not favorited by this user).
	FavoritedAt *time.Time `json:"favoritedAt,omitempty"`

	// Faces holds the names of the named persons detected in this asset.
	// Populated by the favorites listing (List/Top) so the UI can group and
	// filter favorited photos by person; empty elsewhere.
	Faces []string `json:"faces,omitempty"`

	// Soft-delete (trash) related: DeletedAt non-nil means it's in the trash;
	// OriginalPath is the original file_path before soft delete, used for
	// restore and displaying the source folder name.
	DeletedAt    *time.Time `json:"deletedAt,omitempty"`
	OriginalPath string     `json:"originalPath,omitempty"`

	// MatchScore is the semantic-search similarity in [0,1], populated only by
	// SmartSearch (nil elsewhere). Derived from the CLIP embedding L2 distance
	// over unit vectors: sim = 1 - d²/2.
	MatchScore *float64 `json:"matchScore,omitempty"`

	// IsNew is a per-user, per-smart-view annotation set only by
	// SmartViewService.MatchedAssets: true until the requesting user opens the
	// asset after it matched (no asset_views row at/after matched_at). Drives
	// the "New" tag on the Recently-added grid; viewing dismisses it for good.
	IsNew bool `json:"isNew,omitempty"`

	// MatchedBy marks how a SmartSearch result matched: "ocr" when the query hit
	// the asset's recognized text (asset_ocr), "semantic" for CLIP embedding
	// matches. Empty everywhere outside search. A search-time annotation, not a
	// stored asset property — the client uses it for the OCR badge and file-type
	// filter.
	MatchedBy string `json:"matchedBy,omitempty"`

	// BelowCut marks a SmartSearch result as belonging to the "more results"
	// (folded) tier: a search-time annotation set by applyCutTiering on the
	// tail of the semantic-hit subsequence per semanticCutIndex. OCR hits and
	// best-match-tier semantic hits omit this field (false/omitted), so old
	// clients that ignore it see no behavior change. See
	// docs/superpowers/specs/2026-07-08-search-cut-tiering-design.md.
	BelowCut bool `json:"belowCut,omitempty"`

	// Pinned marks a SmartView-matched asset as manually pinned by the user
	// (smart_view_matches.origin=1): the row survives reconciliation and is
	// scored 1.0. Populated only by SmartViewService.MatchedAssets; false
	// (omitted) elsewhere.
	Pinned bool `json:"pinned,omitempty"`
}

type AssetExif struct {
	AssetID   string  `json:"assetId"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Make      string  `json:"make"`
	Model     string  `json:"model"`
}

type FaceDetection struct {
	ID        string `json:"id"`
	AssetID   string `json:"assetId"`
	BBox      string `json:"bbox"`
	Embedding []byte `json:"-"`
}

type Person struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	CoverAssetID string `json:"coverAssetId,omitempty"`
	CoverFaceID  string `json:"coverFaceId,omitempty"`
	// HeroAssetID is the user-chosen background/hero photo for this person.
	// Empty when not set or when the referenced asset has been soft-deleted.
	HeroAssetID string     `json:"heroAssetId,omitempty"`
	Favorite    bool       `json:"favorite"`
	Relation    string     `json:"relation"`
	Confidence  float64    `json:"confidence"`
	Count       int        `json:"count"`
	FirstSeen   *time.Time `json:"firstSeen,omitempty"`
	LastSeen    *time.Time `json:"lastSeen,omitempty"`
	PlacesCount int        `json:"placesCount"`
}

// PersonRelation is a co-occurrence stats row returned by PersonService.PersonRelations.
type PersonRelation struct {
	PersonID    string `json:"personId"`
	Name        string `json:"name"`
	CoverFaceID string `json:"coverFaceId,omitempty"`
	Count       int    `json:"count"`
}

// MergeSuggestion is a candidate merge pair returned by PersonService.MergeSuggestions.
type MergeSuggestion struct {
	ID         string  `json:"id"`
	FromID     string  `json:"fromId"`
	IntoID     string  `json:"intoId"`
	FromFaceID string  `json:"fromFaceId,omitempty"`
	IntoFaceID string  `json:"intoFaceId,omitempty"`
	IntoName   string  `json:"intoName,omitempty"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// PersonPlace is a GPS point returned by PersonService.PersonPlaces.
type PersonPlace struct {
	Latitude  float64    `json:"latitude"`
	Longitude float64    `json:"longitude"`
	TakenAt   *time.Time `json:"takenAt,omitempty"`
	// PlaceName is the human-readable location derived from asset_geo,
	// assembled as "City, Country" when both are known, "City" when only city
	// is available, or empty when no geo data exists for the asset.
	PlaceName string `json:"placeName,omitempty"`
}

type Album struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"createdAt"`
	CoverAssetID string    `json:"coverAssetId,omitempty"`
	// AssetCount is the album card badge: visible members only (live-photo
	// companion videos, trashed and offline assets excluded), so it always
	// matches the number of items ListAssets returns when the album is opened.
	AssetCount int `json:"assetCount,omitempty"`

	// DateStart / DateEnd are the raw taken_at strings of the earliest and
	// latest dated assets in the album (empty when the album has no dated
	// assets). The UI parses year/month from them to render a span label.
	DateStart string `json:"dateStart,omitempty"`
	DateEnd   string `json:"dateEnd,omitempty"`

	// PhotoCount / VideoCount split the album's visible assets by media type
	// (live-photo companion videos and trashed assets excluded). The UI sums
	// these across all albums for the Albums topbar.
	PhotoCount int `json:"photoCount"`
	VideoCount int `json:"videoCount"`
}

type TimelineGroup struct {
	Year   int     `json:"year"`
	Month  int     `json:"month"`
	Assets []Asset `json:"assets"`
}

type IndexStatus struct {
	Pending    int    `json:"pending"`
	Indexed    int    `json:"indexed"`
	Error      int    `json:"error"`
	QueueLen   int    `json:"queueLen"`
	TotalBytes int64  `json:"totalBytes"`
	GalleryDir string `json:"galleryDir,omitempty"`
	DiskTotal  int64  `json:"diskTotal,omitempty"`
	DiskAvail  int64  `json:"diskAvail,omitempty"`
	// MLReady reflects whether the nimoos-photos-ml-server backend answers /ping.
	MLReady bool `json:"mlReady"`
	// Offline counts assets currently flagged offline=1 (their removable drive
	// is unplugged). These assets are hidden from every user-facing surface but
	// still count toward Indexed above — Indexed reflects the assets.status
	// column, which offline does not change. The frontend can use this to show
	// a "N photos are on a disconnected drive" hint.
	Offline int `json:"offline"`
}
