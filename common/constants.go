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

	// ── ML model selection ──────────────────────────────────────────────
	// Must match the preloaded model in ml-cache; bump MLModelGen whenever
	// either model changes. On startup the service detects a generation
	// mismatch and automatically triggers a full rebuild (see service/rebuild.go).
	// SigLIP2 SO400M: measurably outperforms nllb-clip-large on short-word
	// and mixed CN/EN queries (nllb has no discrimination power on single-word
	// queries — "wolf" ranks above "child"); the vision tower is also SO400M.
	CLIPModelName = "ViT-SO400M-16-SigLIP2-384__webli"
	CLIPDim       = 1152              // SO400M output dimension (vec0 table dimension)
	FaceModelName = "antelopev2"      // InsightFace ResNet100@Glint360K
	FaceDim       = 512               // antelopev2 embedding dimension
	OCRModelName  = "PP-OCRv5_server" // PaddleOCR v5 server variant
	// MLModelGen identifies the current model generation; if photos_meta.ml_model_gen
	// doesn't match, a full rebuild is triggered automatically.
	// gen 1 = ViT-B-32__openai + buffalo_l + PP-OCRv5_mobile (implicit; old DBs have no such key).
	// gen 2 = nllb-clip-large-siglip__v1 + antelopev2 + PP-OCRv5_server.
	// gen 3 = ViT-SO400M-16-SigLIP2-384__webli + antelopev2 + PP-OCRv5_server.
	// gen 4 = no model change; forces a full face regeneration so every face
	// row carries a detector score/frontality/sharpness (older rows predate
	// those columns), then re-clusters at ClusterEpsilon 0.48 and re-ranks
	// every person's cover through the unified quality-weighted selection
	// path (see selectCoverFace). This intentionally drops all existing
	// persons, including user-named ones, since names can't be reattached
	// across a from-scratch re-cluster.
	MLModelGen = "4"
)

// TUS upload staging
const (
	// LegacyStagingDir was the hardcoded staging directory before 2026-07;
	// the current directory now follows photos.DataPath (see main.go's reset
	// of StagingDir) — this is kept only to sweep up leftovers on startup.
	LegacyStagingDir = "/DATA/.system_data/photos-tus-staging"
	MaxUploadSize    = int64(20 * 1024 * 1024 * 1024) // 20 GB
	StagingMaxAge    = 7 * 24                         // hours

	// V1TUSPath is the Gateway prefix that must be registered so the resumable
	// upload endpoint at /v1/upload-tus reaches this service.
	V1TUSPath = "/v1/upload-tus"
)

// StagingDir is the tus upload staging directory. var rather than const:
// main.go resets it to <DataPath>/tus-staging after config.Init, so staging
// lives on the same volume as derived data (it moves along if DataPath is
// relocated, instead of permanently occupying the system disk); the
// completion-side rename already has a cross-device copy fallback
// (route/v1/tus.go). The default keeps the legacy path so tests can redirect it.
var StagingDir = LegacyStagingDir
