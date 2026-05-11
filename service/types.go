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
	IndexedAt        *time.Time `json:"indexedAt,omitempty"`
	Status           string     `json:"status"`
	Checksum         string     `json:"checksum,omitempty"`
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
}

type Album struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"createdAt"`
	CoverAssetID string    `json:"coverAssetId,omitempty"`
	AssetCount   int       `json:"assetCount,omitempty"`
}

type TimelineGroup struct {
	Year   int     `json:"year"`
	Month  int     `json:"month"`
	Assets []Asset `json:"assets"`
}

type IndexStatus struct {
	Pending  int `json:"pending"`
	Indexed  int `json:"indexed"`
	Error    int `json:"error"`
	QueueLen int `json:"queueLen"`
}
