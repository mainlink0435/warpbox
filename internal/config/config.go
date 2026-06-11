// Package config manages the YAML-driven configuration for Warpbox.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// TorBoxConfig holds connection details for the TorBox API.
type TorBoxConfig struct {
	APIKey string `yaml:"api_key"` // Required
}

// ServerConfig holds the WebDAV server settings.
type ServerConfig struct {
	ListenAddr string `yaml:"listen_addr"` // Default: ":8080"
	WebDAVRoot string `yaml:"webdav_root"` // Default: "/webdav"
}

// CacheConfig holds JIT RAM buffering parameters.
type CacheConfig struct {
	ChunkSizeMB        int    `yaml:"chunk_size_mb"`         // Default: 16
	MaxRAMMB           int    `yaml:"max_ram_mb"`            // Default: 512
	TTLSeconds         int    `yaml:"ttl_seconds"`           // Default: 30
	EvictionStrategy   string `yaml:"eviction_strategy"`     // "ttl" or "lru"; default: "ttl"
	CDNURLTTLMinutes   int    `yaml:"cdn_url_ttl_minutes"`   // How long to cache CDN URLs; default: 120
	CDNURLAutoRepair   *bool  `yaml:"cdn_url_auto_repair"`   // Auto-repair stale CDN URLs; nil→default true
	CDNURLRepairRetries *int  `yaml:"cdn_url_repair_retries"` // Max CDN proxy retries per request; nil→default 2

	// CDN URL fetch retry settings (for TorBox API errors, not CDN proxy errors).
	// These control how getCDNURLWithRetry behaves on 5xx/429 from TorBox.
	CDNURLRetryBackoff *int `yaml:"cdn_url_retry_backoff"`   // Backoff base in seconds (e.g. 1 → 1s, 2s, 4s); nil→default 1
	CDNURLRetryCount   *int `yaml:"cdn_url_retry_attempts"`  // Max retry attempts; nil→default 1

	// Negative cache: prevents Plex retry loop from hitting TorBox for recently-failed files.
	NegativeCacheTTLSeconds *int `yaml:"negative_cache_ttl_seconds"` // How long to cache failed results; nil→default 30

	// Circuit breaker: per-torrent failure tracking. Torrents exceeding the threshold
	// are marked stale and skipped for all files within until stale period expires.
	CircuitBreakerFailures  *int `yaml:"circuit_breaker_failures"`   // Max failures in window before stalling; nil→default 5
	CircuitBreakerWindowSec *int `yaml:"circuit_breaker_window_seconds"` // Sliding window for failure count; nil→default 60
	CircuitBreakerStaleMin  *int `yaml:"circuit_breaker_stale_minutes"` // Duration a stale torrent is skipped; nil→default 5
}

// ThrottleConfig holds rate-limiting settings.
type ThrottleConfig struct {
	RequestsPerMinute int `yaml:"requests_per_minute"` // Default: 250
}

// LoggingConfig holds logging preferences.
type LoggingConfig struct {
	Format string `yaml:"format"` // "text" or "json"; default: "text"
	Level  string `yaml:"level"`  // "debug", "info", "warn", or "error"; default: "info"
}

// SyncConfig holds metadata sync settings.
type SyncConfig struct {
	IntervalMinutes int `yaml:"interval_minutes"` // Default: 5
	Limit           int `yaml:"limit"`            // Max files to fetch per sync; default: 5000
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
	if c.Server.ListenAddr == "" {
		c.Server.ListenAddr = ":1412"
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
	if c.Cache.EvictionStrategy == "" {
		c.Cache.EvictionStrategy = "ttl"
	}
	if c.Cache.CDNURLTTLMinutes == 0 {
		c.Cache.CDNURLTTLMinutes = 120
	}
	if c.Throttle.RequestsPerMinute == 0 {
		c.Throttle.RequestsPerMinute = 250
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "text"
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Sync.IntervalMinutes == 0 {
		c.Sync.IntervalMinutes = 5
	}
	if c.Sync.Limit == 0 {
		c.Sync.Limit = 5000
	}
	if c.Cache.CDNURLAutoRepair == nil {
		t := true
		c.Cache.CDNURLAutoRepair = &t
	}
	if c.Cache.CDNURLRepairRetries == nil {
		n := 2
		c.Cache.CDNURLRepairRetries = &n
	}
	if c.Cache.CDNURLRetryBackoff == nil {
		n := 1
		c.Cache.CDNURLRetryBackoff = &n
	}
	if c.Cache.CDNURLRetryCount == nil {
		n := 1
		c.Cache.CDNURLRetryCount = &n
	}
	if c.Cache.NegativeCacheTTLSeconds == nil {
		n := 30
		c.Cache.NegativeCacheTTLSeconds = &n
	}
	if c.Cache.CircuitBreakerFailures == nil {
		n := 5
		c.Cache.CircuitBreakerFailures = &n
	}
	if c.Cache.CircuitBreakerWindowSec == nil {
		n := 60
		c.Cache.CircuitBreakerWindowSec = &n
	}
	if c.Cache.CircuitBreakerStaleMin == nil {
		n := 5
		c.Cache.CircuitBreakerStaleMin = &n
	}
}

// validate checks that required fields are present.
func validate(c *Config) error {
	if c.TorBox.APIKey == "" {
		return fmt.Errorf("torbox.api_key is required")
	}
	if c.Cache.EvictionStrategy != "ttl" && c.Cache.EvictionStrategy != "lru" {
		return fmt.Errorf("cache.eviction_strategy must be \"ttl\" or \"lru\", got %q", c.Cache.EvictionStrategy)
	}
	if _, err := ParseLevel(c.Logging.Level); err != nil {
		return fmt.Errorf("logging.level: %w", err)
	}
	if c.Cache.CDNURLRepairRetries != nil {
		r := *c.Cache.CDNURLRepairRetries
		if r < 0 || r > 10 {
			return fmt.Errorf("cache.cdn_url_repair_retries must be 0–10, got %d", r)
		}
	}
	if c.Cache.CDNURLRetryCount != nil {
		r := *c.Cache.CDNURLRetryCount
		if r < 0 || r > 10 {
			return fmt.Errorf("cache.cdn_url_retry_attempts must be 0–10, got %d", r)
		}
	}
	if c.Cache.CDNURLRetryBackoff != nil {
		r := *c.Cache.CDNURLRetryBackoff
		if r < 1 || r > 60 {
			return fmt.Errorf("cache.cdn_url_retry_backoff must be 1–60, got %d", r)
		}
	}
	if c.Cache.NegativeCacheTTLSeconds != nil {
		r := *c.Cache.NegativeCacheTTLSeconds
		if r < 1 || r > 300 {
			return fmt.Errorf("cache.negative_cache_ttl_seconds must be 1–300, got %d", r)
		}
	}
	if c.Cache.CircuitBreakerFailures != nil {
		r := *c.Cache.CircuitBreakerFailures
		if r < 1 || r > 100 {
			return fmt.Errorf("cache.circuit_breaker_failures must be 1–100, got %d", r)
		}
	}
	if c.Cache.CircuitBreakerWindowSec != nil {
		r := *c.Cache.CircuitBreakerWindowSec
		if r < 1 || r > 3600 {
			return fmt.Errorf("cache.circuit_breaker_window_seconds must be 1–3600, got %d", r)
		}
	}
	if c.Cache.CircuitBreakerStaleMin != nil {
		r := *c.Cache.CircuitBreakerStaleMin
		if r < 1 || r > 60 {
			return fmt.Errorf("cache.circuit_breaker_stale_minutes must be 1–60, got %d", r)
		}
	}
	return nil
}

// ParseLevel converts a string log level to slog.Level.
// Valid values: "debug", "info", "warn", "error".
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid level %q: must be debug, info, warn, or error", s)
	}
}

// GenerateTemplate writes a default config.yml to the given path if the
// file does not already exist. The template content is read from
// "config.yml.example" in the same directory as the target path.
// Returns true if a new file was created.
func GenerateTemplate(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil // file already exists
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("checking config file: %w", err)
	}

	// Read the example template from beside the target path.
	examplePath := filepath.Join(filepath.Dir(path), "config.yml.example")
	template, err := os.ReadFile(examplePath)
	if err != nil {
		return false, fmt.Errorf("reading example config from %s: %w", examplePath, err)
	}

	if err := os.WriteFile(path, template, 0644); err != nil {
		return false, fmt.Errorf("writing default config: %w", err)
	}
	return true, nil
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