// Package config manages the YAML-driven configuration for Warpbox.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// TorBoxConfig holds connection details for the TorBox API.
type TorBoxConfig struct {
	APIKey  string `yaml:"api_key"`  // Required
	BaseURL string `yaml:"base_url"` // Default: https://api.torbox.app/v1
}

// ServerConfig holds the WebDAV server settings.
type ServerConfig struct {
	ListenAddr string `yaml:"listen_addr"` // Default: ":8080"
	WebDAVRoot string `yaml:"webdav_root"` // Default: "/webdav"
}

// CacheConfig holds JIT RAM buffering parameters.
type CacheConfig struct {
	ChunkSizeMB  int `yaml:"chunk_size_mb"`  // Default: 16
	MaxRAMMB     int `yaml:"max_ram_mb"`     // Default: 512
	TTLSeconds   int `yaml:"ttl_seconds"`    // Default: 30
}

// ThrottleConfig holds rate-limiting settings.
type ThrottleConfig struct {
	RequestsPerMinute int `yaml:"requests_per_minute"` // Default: 250
}

// LoggingConfig holds logging preferences.
type LoggingConfig struct {
	Format string `yaml:"format"` // "text" or "json"; default: "text"
}

// SyncConfig holds metadata sync settings.
type SyncConfig struct {
	IntervalMinutes int `yaml:"interval_minutes"` // Default: 5
}

// Config is the top-level Warpbox configuration.
type Config struct {
	TorBox   TorBoxConfig   `yaml:"torbox"`
	Server   ServerConfig   `yaml:"server"`
	Cache    CacheConfig    `yaml:"cache"`
	Throttle ThrottleConfig `yaml:"throttle"`
	Logging  LoggingConfig  `yaml:"logging"`
	Sync     SyncConfig     `yaml:"sync"`
}

// setDefaults fills in default values for any zero-valued fields.
func setDefaults(c *Config) {
	if c.TorBox.BaseURL == "" {
		c.TorBox.BaseURL = "https://api.torbox.app/v1"
	}
	if c.Server.ListenAddr == "" {
		c.Server.ListenAddr = ":8080"
	}
	if c.Server.WebDAVRoot == "" {
		c.Server.WebDAVRoot = "/webdav"
	}
	if c.Cache.ChunkSizeMB == 0 {
		c.Cache.ChunkSizeMB = 16
	}
	if c.Cache.MaxRAMMB == 0 {
		c.Cache.MaxRAMMB = 512
	}
	if c.Cache.TTLSeconds == 0 {
		c.Cache.TTLSeconds = 30
	}
	if c.Throttle.RequestsPerMinute == 0 {
		c.Throttle.RequestsPerMinute = 250
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "text"
	}
	if c.Sync.IntervalMinutes == 0 {
		c.Sync.IntervalMinutes = 5
	}
}

// validate checks that required fields are present.
func validate(c *Config) error {
	if c.TorBox.APIKey == "" {
		return fmt.Errorf("torbox.api_key is required")
	}
	return nil
}

// Load reads and parses the YAML config file at the given path.
// It applies defaults for any missing optional fields.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	setDefaults(cfg)

	if err := validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}