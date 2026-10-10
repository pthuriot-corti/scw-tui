package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/scaleway/scaleway-sdk-go/scw"
)

// Config is the persistent TUI configuration loaded from config.json.
type Config struct {
	RefreshIntervalSeconds int    `json:"refreshIntervalSeconds"`
	DefaultRegion           string `json:"defaultRegion"`
	AccentColor             string `json:"accentColor"`
}

const defaultRefreshInterval = 5

func defaultConfig() Config {
	return Config{
		RefreshIntervalSeconds: defaultRefreshInterval,
		DefaultRegion:           string(scw.RegionFrPar),
		AccentColor:             "#8344E5",
	}
}

// configPath returns the path to config.json next to the binary's working directory.
func configPath() string {
	return filepath.Join(".", "config.json")
}

// loadConfig reads config.json; returns defaults if missing or invalid.
func loadConfig() Config {
	cfg := defaultConfig()

	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}

	// Only override fields that are present and valid.
	parsed := Config{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return cfg
	}

	if parsed.RefreshIntervalSeconds > 0 {
		cfg.RefreshIntervalSeconds = parsed.RefreshIntervalSeconds
	}
	if parsed.DefaultRegion != "" {
		region := scw.Region(parsed.DefaultRegion)
		if region.Exists() {
			cfg.DefaultRegion = parsed.DefaultRegion
		}
	}
	if parsed.AccentColor != "" {
		cfg.AccentColor = parsed.AccentColor
	}

	return cfg
}
