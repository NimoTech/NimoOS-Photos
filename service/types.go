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

	// Joined from asset_favorites (populated by List/Timeline/GetAsset when caller
	// supplies a user_id; nil = not favorited by this user).
	FavoritedAt *time.Time `json:"favoritedAt,omitempty"`

	// 软删除（回收站）相关：DeletedAt 非 nil 表示在回收站；
	// OriginalPath 为软删除前的原始 file_path，用于恢复与来源文件夹名展示。
	DeletedAt    *time.Time `json:"deletedAt,omitempty"`
	OriginalPath string     `json:"originalPath,omitempty"`
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
	Pending    int    `json:"pending"`
	Indexed    int    `json:"indexed"`
	Error      int    `json:"error"`
	QueueLen   int    `json:"queueLen"`
	TotalBytes int64  `json:"totalBytes"`
	GalleryDir string `json:"galleryDir,omitempty"`
	DiskTotal  int64  `json:"diskTotal,omitempty"`
	DiskAvail  int64  `json:"diskAvail,omitempty"`
}
