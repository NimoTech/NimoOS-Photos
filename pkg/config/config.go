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
	if len(watchDirs) == 0 {
		watchDirs = []string{"/DATA/Gallery", "/DATA/Documents", "/DATA/Downloads"}
	}
	Cfg = &Config{
		RuntimePath:   v.GetString("common.RuntimePath"),
		LogPath:       v.GetString("common.LogPath"),
		DataPath:      v.GetString("photos.DataPath"),
		MLEndpoint:    v.GetString("photos.MLEndpoint"),
		Workers:       v.GetInt("photos.Workers"),
		WatchDirs:     watchDirs,
		RetentionDays: v.GetInt("photos.RetentionDays"),
		ScanInterval:  v.GetInt("photos.ScanInterval"),
		FacesEnabled:     v.GetBool("photos.FacesEnabled"),
		ScenesEnabled:    v.GetBool("photos.ScenesEnabled"),
		OCREnabled:       v.GetBool("photos.OCREnabled"),
		SmartViewEnabled: v.GetBool("photos.SmartViewEnabled"),
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
	// 扫描间隔（分钟）；配置无此 key 时默认 1440（24h）。0 = 关闭周期重扫。
	if !v.IsSet("photos.ScanInterval") {
		Cfg.ScanInterval = 1440
	}
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
