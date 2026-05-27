package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/NimoTech/NimoOS-Photos/common"
	"github.com/spf13/viper"
)

var Cfg *Config

type Config struct {
	RuntimePath   string
	LogPath       string
	DataPath      string
	MLEndpoint    string
	Workers       int
	WatchDirs     []string
	RetentionDays int
}

func Init(configFile, confSample string) error {
	if configFile == "" {
		configFile = "/etc/nimoos/photos.conf"
	}
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
	return nil
}

// Save writes watchDirs and retentionDays back to the config file and updates Cfg.
func Save(watchDirs []string, retentionDays int) error {
	if Cfg == nil {
		return fmt.Errorf("config not initialized")
	}
	v := viper.New()
	v.SetConfigFile("/etc/nimoos/photos.conf")
	v.SetConfigType("ini")
	_ = v.ReadInConfig()
	v.Set("photos.WatchDirs", strings.Join(watchDirs, ","))
	if retentionDays > 0 {
		v.Set("photos.RetentionDays", retentionDays)
	}
	if err := v.WriteConfig(); err != nil {
		return fmt.Errorf("config.Save: %w", err)
	}
	Cfg.WatchDirs = watchDirs
	if retentionDays > 0 {
		Cfg.RetentionDays = retentionDays
	}
	return nil
}
