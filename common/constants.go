package common

// PhotosVersion is injected at build time via -ldflags; defaults to "dev".
var PhotosVersion = "dev"

const (
	URLFileName = "photos.url"
	Localhost   = "127.0.0.1"
	V1APIPath   = "/v1/photos"
	V1DocPath   = "/doc/v1/photos"

	DefaultMLEndpoint = "http://127.0.0.1:3003"
	DefaultWorkers    = 3

	ThumbSmallSize = 250
	ThumbLargeSize = 1280

	// ── ML 模型选型 ──────────────────────────────────────────────────────
	// 与 ml-cache 预置模型必须一致;换任何一个模型时同步 bump MLModelGen,
	// 服务启动时检测到代次变化会自动触发全量重建(见 service/rebuild.go)。
	// SigLIP2 SO400M:短词/中英混搜判别力实测全面优于 nllb-clip-large
	// (nllb 在单词式查询上无判别力,狼会排到小孩前面),视觉塔同为 SO400M。
	CLIPModelName = "ViT-SO400M-16-SigLIP2-384__webli"
	CLIPDim       = 1152 // SO400M 输出维度(vec0 表维度)
	FaceModelName = "antelopev2"                 // InsightFace ResNet100@Glint360K
	FaceDim       = 512                          // antelopev2 embedding 维度
	OCRModelName  = "PP-OCRv5_server"            // PaddleOCR v5 server 版
	// MLModelGen 标识当前模型代次;photos_meta.ml_model_gen 与之不符时自动全量重建。
	// gen 1 = ViT-B-32__openai + buffalo_l + PP-OCRv5_mobile(隐式,老库无此键)。
	// gen 2 = nllb-clip-large-siglip__v1 + antelopev2 + PP-OCRv5_server。
	MLModelGen = "3"
)

// TUS upload staging
const (
	StagingDir    = "/DATA/.system_data/photos-tus-staging"
	MaxUploadSize = int64(20 * 1024 * 1024 * 1024) // 20 GB
	StagingMaxAge = 7 * 24                         // hours

	// V1TUSPath is the Gateway prefix that must be registered so the resumable
	// upload endpoint at /v1/upload-tus reaches this service.
	V1TUSPath = "/v1/upload-tus"
)
