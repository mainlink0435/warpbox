// Package config manages the YAML-driven configuration for Warpbox.
package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// TorBoxConfig holds connection details for the TorBox API.
type TorBoxConfig struct {
	APIKey string `yaml:"api_key"` // Required
}

// ServerConfig holds the WebDAV server settings.
type ServerConfig struct {
	ListenAddr  string `yaml:"listen_addr"`  // Default: ":1412"
	EnablePprof bool  `yaml:"enable_pprof"` // Enable /debug/pprof/; default false
}

// CacheConfig holds caching and CDN proxy parameters.
type CacheConfig struct {
	CDNURLTTLMinutes    int    `yaml:"cdn_url_ttl_minutes"`    // How long to cache CDN URLs; default: 120
	CDNURLAutoRepair    *bool  `yaml:"cdn_url_auto_repair"`    // Auto-repair stale CDN URLs; nil→default true
	CDNURLRepairRetries *int   `yaml:"cdn_url_repair_retries"` // Max CDN proxy retries per request; nil→default 2

	// CDN URL fetch retry settings (for TorBox API errors, not CDN proxy errors).
	CDNURLRetryBackoff *int `yaml:"cdn_url_retry_backoff"`  // Backoff base in seconds; nil→default 1
	CDNURLRetryCount   *int `yaml:"cdn_url_retry_attempts"` // Max retry attempts; nil→default 1

	// Negative cache: prevents Plex retry loop from hitting TorBox for recently-failed files.
	NegativeCacheTTLSeconds *int `yaml:"negative_cache_ttl_seconds"` // How long to cache failed results; nil→default 30

	// Circuit breaker: per-torrent failure tracking.
	CircuitBreakerFailures   *int `yaml:"circuit_breaker_failures"`    // Max failures in window; nil→default 5
	CircuitBreakerWindowSec  *int `yaml:"circuit_breaker_window_seconds"` // Sliding window; nil→default 60
	CircuitBreakerStaleMin   *int `yaml:"circuit_breaker_stale_minutes"` // Stale duration; nil→default 5

	// Memory management settings.
	NegativeCacheMaxEntries   *int `yaml:"negative_cache_max_entries"`   // Max entries in negative cache; nil→default 5000
	CircuitBreakerMaxEntries  *int `yaml:"circuit_breaker_max_entries"`  // Max entries in circuit breaker; nil→default 2000
	CleanupIntervalSeconds    *int `yaml:"cleanup_interval_seconds"`     // Sweep interval; nil→default 60

	// CDN proxy settings.
	MaxCDNConnections *int `yaml:"max_cdn_connections"` // Max concurrent CDN proxy connections; nil→default 4
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
	IntervalMinutes int  `yaml:"interval_minutes"` // Default: 5
	ListPageSize    int  `yaml:"list_page_size"`   // Items per page when paginating mylist API; default: 5000
	BypassCache     bool `yaml:"bypass_cache"`     // Bypass TorBox cache when fetching torrent list; default false
	RetryAttempts   *int `yaml:"retry_attempts"`   // Max retries for sync API errors; nil→default 3
	RetryBackoff    *int `yaml:"retry_backoff"`    // Base backoff seconds for sync retries; nil→default 1
}

// StatsConfig holds time-series stats collection settings.
type StatsConfig struct {
	IntervalSeconds int `yaml:"interval_seconds"` // How often to record stats snapshots; default 60
	RetentionHours  int `yaml:"retention_hours"`  // How long to retain stats rows; default 24
	ChartMinutes    int `yaml:"chart_minutes"`    // How far back the landing page chart shows; default 60
}

// VirtualPathConfig holds a single virtual path with its filters.
type VirtualPathConfig struct {
	Name             string `yaml:"name"`               // Virtual directory name, e.g. "movies"
	DirectoryInclude string `yaml:"directory_include"`  // Include dirs matching this regex
	DirectoryExclude string `yaml:"directory_exclude"`  // Exclude dirs matching this regex
	FileRegex        string `yaml:"file_regex"`         // Regex applied to file paths within torrents
	LargestFileOnly  bool   `yaml:"largest_file_only"`  // Show only the largest file per torrent
}

// LibraryConfig holds settings for the virtual library feature.
type LibraryConfig struct {
	VirtualPaths     []VirtualPathConfig `yaml:"virtual_paths"`
	OnItemsAdded     string              `yaml:"on_items_added"`   // Shell command for new items
	OnItemsRemoved   string              `yaml:"on_items_removed"` // Shell command for removed items
	HookTimeoutSec   int                 `yaml:"hook_timeout_seconds"` // Hook execution timeout; default 30
}

// AuthConfig holds optional HTTP Basic Authentication settings for the web UI.
type AuthConfig struct {
	Enabled  bool   `yaml:"enabled"`  // Enable Basic Auth for web management UI; default false
	Username string `yaml:"username"` // Default: "admin"
	Password string `yaml:"password"` // Required when enabled
}

// Config is the top-level Warpbox configuration.
type Config struct {
	TorBox   TorBoxConfig   `yaml:"torbox"`
	Server   ServerConfig   `yaml:"server"`
	Cache    CacheConfig    `yaml:"cache"`
	Throttle ThrottleConfig `yaml:"throttle"`
	Logging  LoggingConfig  `yaml:"logging"`
	Sync     SyncConfig     `yaml:"sync"`
	Stats    StatsConfig    `yaml:"stats"`
	Auth     AuthConfig     `yaml:"auth"`
	Library  LibraryConfig  `yaml:"library"`
}

// setDefaults fills in default values for any zero-valued fields.
func setDefaults(c *Config) {
	if c.Server.ListenAddr == "" {
		c.Server.ListenAddr = ":1412"
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
	if c.Sync.ListPageSize == 0 {
		c.Sync.ListPageSize = 5000
	}
	if c.Sync.RetryAttempts == nil {
		n := 3
		c.Sync.RetryAttempts = &n
	}
	if c.Sync.RetryBackoff == nil {
		n := 1
		c.Sync.RetryBackoff = &n
	}
	if c.Stats.IntervalSeconds == 0 {
		c.Stats.IntervalSeconds = 60
	}
	if c.Stats.RetentionHours == 0 {
		c.Stats.RetentionHours = 24
	}
	if c.Stats.ChartMinutes == 0 {
		c.Stats.ChartMinutes = 60
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
	if c.Cache.NegativeCacheMaxEntries == nil {
		n := 5000
		c.Cache.NegativeCacheMaxEntries = &n
	}
	if c.Cache.CircuitBreakerMaxEntries == nil {
		n := 2000
		c.Cache.CircuitBreakerMaxEntries = &n
	}
	if c.Cache.CleanupIntervalSeconds == nil {
		n := 60
		c.Cache.CleanupIntervalSeconds = &n
	}
	if c.Cache.MaxCDNConnections == nil {
		n := 4
		c.Cache.MaxCDNConnections = &n
	}
	if c.Auth.Username == "" {
		c.Auth.Username = "admin"
	}
	if c.Library.HookTimeoutSec == 0 {
		c.Library.HookTimeoutSec = 30
	}
}

// validate checks that required fields are present.
func validate(c *Config) error {
	if c.TorBox.APIKey == "" {
		return fmt.Errorf("torbox.api_key is required")
	}
	if err := ParseFormat(c.Logging.Format); err != nil {
		return fmt.Errorf("logging.format: %w", err)
	}
	if _, err := ParseLevel(c.Logging.Level); err != nil {
		return fmt.Errorf("logging.level: %w", err)
	}
	if c.Cache.CDNURLTTLMinutes < 1 || c.Cache.CDNURLTTLMinutes > 1440 {
		return fmt.Errorf("cache.cdn_url_ttl_minutes must be 1–1440, got %d", c.Cache.CDNURLTTLMinutes)
	}
	if c.Sync.IntervalMinutes < 1 || c.Sync.IntervalMinutes > 1440 {
		return fmt.Errorf("sync.interval_minutes must be 1–1440, got %d", c.Sync.IntervalMinutes)
	}
	if c.Sync.ListPageSize < 1 || c.Sync.ListPageSize > 10000 {
		return fmt.Errorf("sync.list_page_size must be 1–10000, got %d", c.Sync.ListPageSize)
	}
	if c.Sync.RetryAttempts != nil {
		r := *c.Sync.RetryAttempts
		if r < 0 || r > 10 {
			return fmt.Errorf("sync.retry_attempts must be 0–10, got %d", r)
		}
	}
	if c.Sync.RetryBackoff != nil {
		r := *c.Sync.RetryBackoff
		if r < 1 || r > 60 {
			return fmt.Errorf("sync.retry_backoff must be 1–60, got %d", r)
		}
	}
	if c.Stats.IntervalSeconds < 10 || c.Stats.IntervalSeconds > 3600 {
		return fmt.Errorf("stats.interval_seconds must be 10–3600, got %d", c.Stats.IntervalSeconds)
	}
	if c.Stats.RetentionHours < 1 || c.Stats.RetentionHours > 720 {
		return fmt.Errorf("stats.retention_hours must be 1–720, got %d", c.Stats.RetentionHours)
	}
	if c.Stats.ChartMinutes < 1 || c.Stats.ChartMinutes > 1440 {
		return fmt.Errorf("stats.chart_minutes must be 1–1440, got %d", c.Stats.ChartMinutes)
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
	if c.Cache.NegativeCacheMaxEntries != nil {
		r := *c.Cache.NegativeCacheMaxEntries
		if r < 100 || r > 50000 {
			return fmt.Errorf("cache.negative_cache_max_entries must be 100–50000, got %d", r)
		}
	}
	if c.Cache.CircuitBreakerMaxEntries != nil {
		r := *c.Cache.CircuitBreakerMaxEntries
		if r < 50 || r > 20000 {
			return fmt.Errorf("cache.circuit_breaker_max_entries must be 50–20000, got %d", r)
		}
	}
	if c.Cache.CleanupIntervalSeconds != nil {
		r := *c.Cache.CleanupIntervalSeconds
		if r < 10 || r > 3600 {
			return fmt.Errorf("cache.cleanup_interval_seconds must be 10–3600, got %d", r)
		}
	}
	if c.Cache.MaxCDNConnections != nil {
		r := *c.Cache.MaxCDNConnections
		if r < 1 || r > 64 {
			return fmt.Errorf("cache.max_cdn_connections must be 1–64, got %d", r)
		}
	}
	if c.Throttle.RequestsPerMinute < 10 || c.Throttle.RequestsPerMinute > 1000 {
		return fmt.Errorf("throttle.requests_per_minute must be 10–1000, got %d", c.Throttle.RequestsPerMinute)
	}
	if c.Auth.Enabled && c.Auth.Password == "" {
		return fmt.Errorf("auth.password is required when auth.enabled is true")
	}
	if err := validateLibrary(&c.Library); err != nil {
		return err
	}
	return nil
}

func validateLibrary(l *LibraryConfig) error {
	seen := make(map[string]bool)
	for i, vp := range l.VirtualPaths {
		name := vp.Name
		if name == "" {
			return fmt.Errorf("library.virtual_paths[%d].name is required", i)
		}
		if strings.Contains(name, "/") {
			return fmt.Errorf("library.virtual_paths[%d].name must not contain '/', got %q", i, name)
		}
		if name == "__all__" {
			continue
		}
		if seen[name] {
			return fmt.Errorf("library.virtual_paths[%d].name %q is duplicated", i, name)
		}
		seen[name] = true

		if vp.DirectoryInclude != "" {
			if _, err := regexp.Compile(vp.DirectoryInclude); err != nil {
				return fmt.Errorf("library.virtual_paths[%d].directory_include: %w", i, err)
			}
		}
		if vp.DirectoryExclude != "" {
			if _, err := regexp.Compile(vp.DirectoryExclude); err != nil {
				return fmt.Errorf("library.virtual_paths[%d].directory_exclude: %w", i, err)
			}
		}
		if vp.FileRegex != "" {
			if _, err := regexp.Compile(vp.FileRegex); err != nil {
				return fmt.Errorf("library.virtual_paths[%d].file_regex: %w", i, err)
			}
		}
	}
	if l.HookTimeoutSec < 1 || l.HookTimeoutSec > 3600 {
		return fmt.Errorf("library.hook_timeout_seconds must be 1–3600, got %d", l.HookTimeoutSec)
	}
	return nil
}

// ParseFormat validates a logging format string.
func ParseFormat(s string) error {
	switch strings.ToLower(s) {
	case "text", "json":
		return nil
	default:
		return fmt.Errorf("invalid format %q: must be text or json", s)
	}
}

// ParseLevel converts a string log level to slog.Level.
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
func GenerateTemplate(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("checking config file: %w", err)
	}

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

// UpdateLogLevel reads the config file, updates the logging level, and writes
// it back atomically (temp file + rename). Uses yaml.Node to preserve
// comments, formatting, and structure on round-trip.
func UpdateLogLevel(path string, newLevel string) error {
	// Validate the level first.
	_, err := ParseLevel(newLevel)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	// Parse into a yaml.Node tree to preserve comments on round-trip.
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	// Navigate to the top-level mapping.
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return fmt.Errorf("unexpected YAML structure: expected a document with one root node")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("unexpected YAML structure: expected root mapping")
	}

	// Walk the root mapping to find the "logging" key.
	var changed bool
	var loggingMapping *yaml.Node
	for i := 0; i < len(root.Content)-1; i += 2 {
		if root.Content[i].Value == "logging" {
			loggingMapping = root.Content[i+1]
			break
		}
	}

	if loggingMapping != nil {
		changed = updateLevelInConfig(loggingMapping, newLevel)
	} else {
		appendLoggingSection(root, newLevel)
		changed = true
	}

	if !changed {
		return nil
	}

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}
	encoder.Close()
	out := buf.Bytes()

	// Atomic write: temp file + rename.
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, out, 0644); err != nil {
		return fmt.Errorf("writing temp config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp config: %w", err)
	}

	return nil
}

// updateLevelInConfig finds the "level" key in a mapping node and sets its
// value. Returns true if a change was made.
func updateLevelInConfig(mapping *yaml.Node, newLevel string) bool {
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == "level" {
			if mapping.Content[i+1].Value == newLevel {
				return false
			}
			mapping.Content[i+1].Value = newLevel
			return true
		}
	}
	levelKey := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "level"}
	levelVal := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: newLevel}
	mapping.Content = append(mapping.Content, levelKey, levelVal)
	return true
}

// appendLoggingSection appends a logging section at the end of the root
// mapping with a descriptive comment.
func appendLoggingSection(root *yaml.Node, newLevel string) {
	levelKey := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "level"}
	levelVal := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: newLevel}
	loggingMapping := &yaml.Node{
		Kind:    yaml.MappingNode,
		Tag:     "!!map",
		Content: []*yaml.Node{levelKey, levelVal},
	}
	loggingKey := &yaml.Node{
		Kind:        yaml.ScalarNode,
		Tag:         "!!str",
		Value:       "logging",
		HeadComment: "Log level set via web UI",
	}
	root.Content = append(root.Content, loggingKey, loggingMapping)
}

// Load reads and parses the YAML config file at the given path.
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