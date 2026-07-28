package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/spf13/viper"
)

var Cfg *Config

// configPath is the file Init loaded from; Save writes back to the same file.
var configPath = "/etc/nimoos/photos.conf"

type Config struct {
	RuntimePath      string
	LogPath          string
	DataPath         string
	MLEndpoint       string
	Workers          int
	WatchDirs        []string
	RetentionDays    int
	ScanInterval     int
	FacesEnabled     bool
	ScenesEnabled    bool
	OCREnabled       bool
	SmartViewEnabled bool
	// AestheticEnabled 控制美学评分:关闭时不内联打分、不跑 BackfillAesthetic;
	// 已有分数保留并继续参与封面排序,新资产分数为 NULL 走各处兜底排序。
	AestheticEnabled bool

	// PreviewPregen 控制视频低码率悬浮预览(preview.mp4,单个可达数十 MB)是否
	// 在索引期/启动补跑时预生成。缺省 false = 纯懒生成:仅当用户真正悬浮预览时
	// 由路由端现场生成(route/v1/assets.go Preview),不为从不预览的视频付出磁盘。
	// sprite.jpg(雪碧图,数百 KB)不受此开关影响,始终预生成。
	PreviewPregen bool

	// MinMatchSimilarity, when > 0, drops SmartSearch results whose (display-scale,
	// post-recalibration) match score is below it. Left at 0 (no filtering) by
	// default — see service/search.go for the full rationale and calibration
	// history of this knob.
	MinMatchSimilarity float64
	// SimDisplayFloor/SimDisplayCeil linearly rescale the raw CLIP cosine
	// similarity into the [0,1] display score shown to the UI. Defaults
	// (0.03/0.13) were empirically calibrated against the current CLIP model
	// (nllb-clip-large-siglip__v1); see service/scan.go's displayScore for
	// details. Only override these after a fresh empirical pass following a
	// model or re-embed change.
	SimDisplayFloor float64
	SimDisplayCeil  float64

	// SearchCutAlpha is the relative-threshold multiplier used by SmartSearch's
	// belowCut tiering: on the semantic-hit subsequence (sorted descending),
	// the first score below SearchCutAlpha × Top-1 marks (one of the two)
	// candidate cut points into the "more results" tier. Defaults to 0.7 (see
	// service/searchcut.go's semanticCutIndex for the full rule, including the
	// cliff-detection signal it combines with).
	SearchCutAlpha float64

	// Doc 分类混合判据(OCR 类)的权重与标定(见 service/docscore.go):
	// DocWSem/DocWGeo 语义与几何权重;DocScoreFloor 判文档的加权分下限;
	// DocSemFloor/DocSemCeil 语义边际线性归一的两个标定端点。
	// 默认值为当前库校准的经验值,换 CLIP 模型代次后需复核。
	DocWSem       float64
	DocWGeo       float64
	DocScoreFloor float64
	DocSemFloor   float64
	DocSemCeil    float64
}

func Init(configFile, confSample string) error {
	if configFile == "" {
		configFile = "/etc/nimoos/photos.conf"
	}
	configPath = configFile
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		if err := os.WriteFile(configFile, []byte(confSample), 0644); err != nil {
			return fmt.Errorf("failed to write default config: %w", err)
		}
	}
	v := viper.New()
	v.SetConfigFile(configFile)
	v.SetConfigType("ini")
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}
	watchRaw := v.GetString("photos.WatchDirs")
	var watchDirs []string
	for _, d := range strings.Split(watchRaw, ",") {
		if d = strings.TrimSpace(d); d != "" {
			watchDirs = append(watchDirs, d)
		}
	}
	// watchDirs 为空 ⇒ watcher 自动模式（范围 = EnumerateScanRoots，即系统盘
	// + 全部已挂载用户分区，动态跟随挂载）；非空 ⇒ 手工监控清单（向后兼容）。
	Cfg = &Config{
		RuntimePath:      v.GetString("common.RuntimePath"),
		LogPath:          v.GetString("common.LogPath"),
		DataPath:         v.GetString("photos.DataPath"),
		MLEndpoint:       v.GetString("photos.MLEndpoint"),
		Workers:          v.GetInt("photos.Workers"),
		WatchDirs:        watchDirs,
		RetentionDays:    v.GetInt("photos.RetentionDays"),
		ScanInterval:     v.GetInt("photos.ScanInterval"),
		FacesEnabled:     v.GetBool("photos.FacesEnabled"),
		ScenesEnabled:    v.GetBool("photos.ScenesEnabled"),
		OCREnabled:       v.GetBool("photos.OCREnabled"),
		SmartViewEnabled: v.GetBool("photos.SmartViewEnabled"),
		AestheticEnabled: v.GetBool("photos.AestheticEnabled"),
		PreviewPregen:    v.GetBool("photos.PreviewPregen"),

		MinMatchSimilarity: v.GetFloat64("photos.MinMatchSimilarity"),
		SimDisplayFloor:    v.GetFloat64("photos.SimDisplayFloor"),
		SimDisplayCeil:     v.GetFloat64("photos.SimDisplayCeil"),
		SearchCutAlpha:     v.GetFloat64("photos.SearchCutAlpha"),

		DocWSem:       v.GetFloat64("photos.DocWSem"),
		DocWGeo:       v.GetFloat64("photos.DocWGeo"),
		DocScoreFloor: v.GetFloat64("photos.DocScoreFloor"),
		DocSemFloor:   v.GetFloat64("photos.DocSemFloor"),
		DocSemCeil:    v.GetFloat64("photos.DocSemCeil"),
	}
	if Cfg.RuntimePath == "" {
		Cfg.RuntimePath = "/var/run/nimoos"
	}
	if Cfg.LogPath == "" {
		Cfg.LogPath = "/var/log/nimoos"
	}
	if Cfg.DataPath == "" {
		Cfg.DataPath = "/DATA/.system_data/photos"
	}
	if Cfg.MLEndpoint == "" {
		Cfg.MLEndpoint = common.DefaultMLEndpoint
	}
	if Cfg.Workers == 0 {
		Cfg.Workers = common.DefaultWorkers
	}
	if Cfg.RetentionDays <= 0 {
		Cfg.RetentionDays = 30
	}
	// FacesEnabled 缺省（配置无此 key）时默认开启。
	if !v.IsSet("photos.FacesEnabled") {
		Cfg.FacesEnabled = true
	}
	// 新开关缺省（配置无此 key）时默认开启，与 FacesEnabled 同语义。
	if !v.IsSet("photos.ScenesEnabled") {
		Cfg.ScenesEnabled = true
	}
	if !v.IsSet("photos.OCREnabled") {
		Cfg.OCREnabled = true
	}
	if !v.IsSet("photos.SmartViewEnabled") {
		Cfg.SmartViewEnabled = true
	}
	if !v.IsSet("photos.AestheticEnabled") {
		Cfg.AestheticEnabled = true
	}
	// 扫描间隔（分钟）；配置无此 key 时默认 1440（24h）。0 = 关闭周期重扫。
	if !v.IsSet("photos.ScanInterval") {
		Cfg.ScanInterval = 1440
	}
	// 语义搜索相关性下限：配置无此 key 时默认 0（不过滤），与改配置化之前的硬编码行为一致。
	if !v.IsSet("photos.MinMatchSimilarity") {
		Cfg.MinMatchSimilarity = 0.0
	}
	// 展示层标定区间端点：配置无此 key 时使用当前模型的经验默认值。
	if !v.IsSet("photos.SimDisplayFloor") {
		Cfg.SimDisplayFloor = 0.03
	}
	if !v.IsSet("photos.SimDisplayCeil") {
		Cfg.SimDisplayCeil = 0.13
	}
	// 语义搜索自适应断层的相对阈值系数：配置无此 key 时默认 0.7。
	if !v.IsSet("photos.SearchCutAlpha") {
		Cfg.SearchCutAlpha = 0.7
	}
	// Doc 分类判据五项(DocWSem/DocWGeo/DocScoreFloor/DocSemFloor/DocSemCeil)
	// 不在此兜底：0 值由 service/docscore.go 的访问器回退经验默认值(simDisplayFloor 同款)。
	return nil
}

// Settings is the subset of Config persisted via the settings API.
type Settings struct {
	WatchDirs        []string
	RetentionDays    int
	ScanInterval     int
	FacesEnabled     bool
	ScenesEnabled    bool
	OCREnabled       bool
	SmartViewEnabled bool
}

// Save writes the settings back to the config file and updates Cfg.
//
// Viper's WriteConfig derives the encoder from the file extension, so files
// ending in ".conf" (not in viper's SupportedExts) would fail.  We work
// around this by writing to a sibling ".ini" temp file and then renaming it
// atomically to the real path.
func Save(s Settings) error {
	if Cfg == nil {
		return fmt.Errorf("config not initialized")
	}
	v := viper.New()
	v.SetConfigFile(configPath)
	v.SetConfigType("ini")
	_ = v.ReadInConfig()
	v.Set("photos.WatchDirs", strings.Join(s.WatchDirs, ","))
	if s.RetentionDays > 0 {
		v.Set("photos.RetentionDays", s.RetentionDays)
	}
	v.Set("photos.FacesEnabled", s.FacesEnabled)
	v.Set("photos.ScenesEnabled", s.ScenesEnabled)
	v.Set("photos.OCREnabled", s.OCREnabled)
	v.Set("photos.SmartViewEnabled", s.SmartViewEnabled)
	v.Set("photos.ScanInterval", s.ScanInterval)
	// WriteConfig uses the file extension to choose the encoder; ".conf" is not
	// in viper's SupportedExts.  Write to a ".ini" temp file then rename.
	tmpPath := configPath + ".ini"
	if err := v.WriteConfigAs(tmpPath); err != nil {
		return fmt.Errorf("config.Save: %w", err)
	}
	if err := os.Rename(tmpPath, configPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("config.Save rename: %w", err)
	}
	Cfg.WatchDirs = s.WatchDirs
	if s.RetentionDays > 0 {
		Cfg.RetentionDays = s.RetentionDays
	}
	Cfg.FacesEnabled = s.FacesEnabled
	Cfg.ScenesEnabled = s.ScenesEnabled
	Cfg.OCREnabled = s.OCREnabled
	Cfg.SmartViewEnabled = s.SmartViewEnabled
	Cfg.ScanInterval = s.ScanInterval
	return nil
}
