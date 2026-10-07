package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type M3UConfig struct {
	URL            string        `toml:"url"`
	AutoUpdate     int           `toml:"auto_update"`
	UpdateInterval time.Duration `toml:"-"`
	DetectMode     string        `toml:"detect_mode"`
}

type Config struct {
	ServerAddr string    `toml:"server_addr"`
	SecretKey  string    `toml:"secret_key"`
	M3U        M3UConfig `toml:"m3u"`
}

const (
	DetectModeURL   = "url"
	DetectModeProbe = "probe"
)

func DefaultConfig() Config {
	return Config{
		ServerAddr: ":8080",
		M3U: M3UConfig{
			URL:            "",
			AutoUpdate:     0,
			UpdateInterval: 0,
			DetectMode:     DetectModeProbe,
		},
	}
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	var cfg Config
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return nil, fmt.Errorf("parse toml config: %w", err)
	}

	if cfg.ServerAddr == "" {
		cfg.ServerAddr = ":8080"
	}
	cfg.ServerAddr = strings.TrimSpace(cfg.ServerAddr)
	cfg.M3U.URL = strings.TrimSpace(cfg.M3U.URL)
	if cfg.M3U.URL != "" {
		parsed, err := url.Parse(cfg.M3U.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("invalid m3u.url: expected an http or https URL")
		}
	}

	switch strings.ToLower(strings.TrimSpace(cfg.M3U.DetectMode)) {
	case DetectModeURL:
		cfg.M3U.DetectMode = DetectModeURL
	case "", DetectModeProbe:
		cfg.M3U.DetectMode = DetectModeProbe
	default:
		return nil, fmt.Errorf("invalid m3u.detect_mode: expected %q or %q", DetectModeURL, DetectModeProbe)
	}

	maxMinutes := int64(time.Duration(1<<63-1) / time.Minute)
	if cfg.M3U.AutoUpdate > 0 && int64(cfg.M3U.AutoUpdate) <= maxMinutes {
		cfg.M3U.UpdateInterval = time.Duration(cfg.M3U.AutoUpdate) * time.Minute
	} else {
		cfg.M3U.AutoUpdate = 0
		cfg.M3U.UpdateInterval = 0
	}

	return &cfg, nil
}
