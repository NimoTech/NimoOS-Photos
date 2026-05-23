package common

const (
	PhotosVersion = "0.1.0"
	URLFileName   = "photos.url"
	Localhost     = "127.0.0.1"
	V1APIPath     = "/v1/photos"
	V1DocPath     = "/doc/v1/photos"

	DefaultMLEndpoint = "http://127.0.0.1:3003"
	DefaultWorkers    = 3

	ThumbSmallSize = 250
	ThumbLargeSize = 1280

	CLIPModelName = "ViT-B-32__openai"
	FaceModelName = "buffalo_l"
)

// TUS upload staging
const (
	StagingDir    = "/DATA/.system_data/photos-tus-staging"
	MaxUploadSize = int64(20 * 1024 * 1024 * 1024) // 20 GB
	StagingMaxAge = 7 * 24                          // hours
)
