package config

import (
	"os"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	// Create minimal config with only required field.
	content := []byte("torbox:\n  api_key: \"test-key-123\"\n")
	tmp := t.TempDir() + "/config.yml"
	if err := os.WriteFile(tmp, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(tmp)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Check required field.
	if cfg.TorBox.APIKey != "test-key-123" {
		t.Errorf("api_key = %q, want %q", cfg.TorBox.APIKey, "test-key-123")
	}

	// Check defaults.
	if cfg.TorBox.BaseURL != "https://api.torbox.app" {
		t.Errorf("base_url = %q, want %q", cfg.TorBox.BaseURL, "https://api.torbox.app")
	}
	if cfg.Server.ListenAddr != ":1412" {
		t.Errorf("listen_addr = %q, want %q", cfg.Server.ListenAddr, ":1412")
	}
	if cfg.Cache.ChunkSizeMB != 16 {
		t.Errorf("chunk_size_mb = %d, want %d", cfg.Cache.ChunkSizeMB, 16)
	}
	if cfg.Cache.MaxRAMMB != 512 {
		t.Errorf("max_ram_mb = %d, want %d", cfg.Cache.MaxRAMMB, 512)
	}
	if cfg.Cache.TTLSeconds != 30 {
		t.Errorf("ttl_seconds = %d, want %d", cfg.Cache.TTLSeconds, 30)
	}
	if cfg.Cache.EvictionStrategy != "ttl" {
		t.Errorf("eviction_strategy = %q, want %q", cfg.Cache.EvictionStrategy, "ttl")
	}
	if cfg.Cache.CDNURLTTLMinutes != 120 {
		t.Errorf("cdn_url_ttl_minutes = %d, want %d", cfg.Cache.CDNURLTTLMinutes, 120)
	}
	if cfg.Throttle.RequestsPerMinute != 250 {
		t.Errorf("requests_per_minute = %d, want %d", cfg.Throttle.RequestsPerMinute, 250)
	}
	if cfg.Logging.Format != "text" {
		t.Errorf("log_format = %q, want %q", cfg.Logging.Format, "text")
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("log_level = %q, want %q", cfg.Logging.Level, "info")
	}
	if cfg.Sync.IntervalMinutes != 5 {
		t.Errorf("sync_interval = %d, want %d", cfg.Sync.IntervalMinutes, 5)
	}
}

func TestLoadCustomValues(t *testing.T) {
	content := []byte(`
torbox:
  api_key: "custom-key"
  base_url: "https://custom.api/torbox"

server:
  listen_addr: "127.0.0.1:9000"
  webdav_root: "/files"

cache:
  chunk_size_mb: 32
  max_ram_mb: 1024
  ttl_seconds: 60
  eviction_strategy: "lru"
  cdn_url_ttl_minutes: 240

throttle:
  requests_per_minute: 100

logging:
  format: "json"

sync:
  interval_minutes: 10
`)
	tmp := t.TempDir() + "/config.yml"
	if err := os.WriteFile(tmp, content, 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(tmp)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.TorBox.APIKey != "custom-key" {
		t.Errorf("api_key = %q", cfg.TorBox.APIKey)
	}
	if cfg.TorBox.BaseURL != "https://custom.api/torbox" {
		t.Errorf("base_url = %q", cfg.TorBox.BaseURL)
	}
	if cfg.Server.ListenAddr != "127.0.0.1:9000" {
		t.Errorf("listen_addr = %q", cfg.Server.ListenAddr)
	}
	if cfg.Server.WebDAVRoot != "/files" {
		t.Errorf("webdav_root = %q", cfg.Server.WebDAVRoot)
	}
	if cfg.Cache.ChunkSizeMB != 32 {
		t.Errorf("chunk_size_mb = %d", cfg.Cache.ChunkSizeMB)
	}
	if cfg.Cache.MaxRAMMB != 1024 {
		t.Errorf("max_ram_mb = %d", cfg.Cache.MaxRAMMB)
	}
	if cfg.Cache.TTLSeconds != 60 {
		t.Errorf("ttl_seconds = %d", cfg.Cache.TTLSeconds)
	}
	if cfg.Cache.EvictionStrategy != "lru" {
		t.Errorf("eviction_strategy = %q", cfg.Cache.EvictionStrategy)
	}
	if cfg.Cache.CDNURLTTLMinutes != 240 {
		t.Errorf("cdn_url_ttl_minutes = %d", cfg.Cache.CDNURLTTLMinutes)
	}
	if cfg.Throttle.RequestsPerMinute != 100 {
		t.Errorf("requests_per_minute = %d", cfg.Throttle.RequestsPerMinute)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("log_format = %q", cfg.Logging.Format)
	}
	if cfg.Sync.IntervalMinutes != 10 {
		t.Errorf("sync_interval = %d", cfg.Sync.IntervalMinutes)
	}
}

func TestLoadMissingAPIKey(t *testing.T) {
	content := []byte("torbox:\n  base_url: \"https://test\"\n")
	tmp := t.TempDir() + "/config.yml"
	if err := os.WriteFile(tmp, content, 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(tmp)
	if err == nil {
		t.Fatal("expected error for missing api_key, got nil")
	}
}

func TestLoadInvalidFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	content := []byte("torbox:\n  api_key: [invalid yaml\n")
	tmp := t.TempDir() + "/config.yml"
	if err := os.WriteFile(tmp, content, 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(tmp)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  string // slog.Level.String()
	}{
		{"debug", "DEBUG"},
		{"info", "INFO"},
		{"warn", "WARN"},
		{"error", "ERROR"},
		{"DEBUG", "DEBUG"},
		{"Info", "INFO"},
	}

	for _, tt := range tests {
		lvl, err := ParseLevel(tt.input)
		if err != nil {
			t.Errorf("ParseLevel(%q): unexpected error: %v", tt.input, err)
			continue
		}
		if lvl.String() != tt.want {
			t.Errorf("ParseLevel(%q) = %s, want %s", tt.input, lvl, tt.want)
		}
	}
}

func TestParseLevelInvalid(t *testing.T) {
	_, err := ParseLevel("verbose")
	if err == nil {
		t.Error("expected error for invalid level 'verbose'")
	}
	_, err = ParseLevel("")
	if err == nil {
		t.Error("expected error for empty level")
	}
}

func TestLoadInvalidLevel(t *testing.T) {
	content := []byte("torbox:\n  api_key: \"key\"\nlogging:\n  level: \"verbose\"\n")
	tmp := t.TempDir() + "/config.yml"
	if err := os.WriteFile(tmp, content, 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(tmp)
	if err == nil {
		t.Fatal("expected error for invalid logging level, got nil")
	}
}

func TestLoadInvalidEvictionStrategy(t *testing.T) {
	content := []byte("torbox:\n  api_key: \"key\"\ncache:\n  eviction_strategy: \"fifo\"\n")
	tmp := t.TempDir() + "/config.yml"
	if err := os.WriteFile(tmp, content, 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(tmp)
	if err == nil {
		t.Fatal("expected error for invalid eviction_strategy, got nil")
	}
}