// Warpbox — WebDAV proxy for TorBox.
//
// Bootstraps configuration, structured logging, metadata store, cache,
// throttle, TorBox API client, and the WebDAV HTTP server.
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ben/warpbox/internal/cache"
	"github.com/ben/warpbox/internal/config"
	"github.com/ben/warpbox/internal/metadata"
	"github.com/ben/warpbox/internal/server"
	"github.com/ben/warpbox/internal/throttle"
	"github.com/ben/warpbox/internal/torbox"
)

//go:embed banner.txt
var banner string

// Version is injected at build time via ldflags (e.g. -X main.Version=v0.6.0).
// Defaults to "dev" for local builds.
var Version = "dev"

func main() {
	configPath := flag.String("config", "config.yml", "Path to the YAML configuration file")
	dbPath := flag.String("db", "warpbox.db", "Path to the SQLite metadata database")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	// Bootstrap a minimal logger so early errors are properly structured.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	// --- Configuration ---
	// Auto-generate a default config if the file doesn't exist, then continue
	// so the user sees the API error and knows what to fix.
	if created, err := config.GenerateTemplate(*configPath); err != nil {
		slog.Error("failed to generate default config", "error", err)
		os.Exit(1)
	} else if created {
		slog.Warn("default config generated — edit it to add your TorBox API key", "path", *configPath)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// --- Structured logging ---
	logLevel, err := config.ParseLevel(cfg.Logging.Level)
	if err != nil {
		slog.Error("invalid logging level", "level", cfg.Logging.Level, "error", err)
		os.Exit(1)
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: logLevel}
	if cfg.Logging.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	// Wrap the handler with a ring buffer for the /logs/ endpoint.
	bufHandler := server.NewRingBufferHandler(handler)
	server.SetLogBuffer(bufHandler)
	logger := slog.New(bufHandler)
	slog.SetDefault(logger)

	fmt.Print(banner)
	fmt.Printf("\nwarpbox %s — WebDAV proxy for TorBox\n\n", Version)

	slog.Info("starting warpbox",
		"version", Version,
		"listen_addr", cfg.Server.ListenAddr,
		"webdav_root", cfg.Server.WebDAVRoot,
		"log_format", cfg.Logging.Format,
		"log_level", cfg.Logging.Level,
	)

	// --- TorBox API client ---
	torBoxClient := torbox.NewClient(cfg.TorBox.BaseURL, cfg.TorBox.APIKey)

	// --- Metadata store (SQLite WAL) ---
	dbDir := filepath.Dir(*dbPath)
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		slog.Error("creating database directory", "dir", dbDir, "error", err)
		os.Exit(1)
	}
	metadataStore, err := metadata.Open(*dbPath)
	if err != nil {
		slog.Error("opening metadata store", "path", *dbPath, "error", err)
		os.Exit(1)
	}
	defer metadataStore.Close()

	// --- RAM cache ---
	evictionStrategy := cache.StrategyTTL
	if cfg.Cache.EvictionStrategy == "lru" {
		evictionStrategy = cache.StrategyLRU
	}
	ramCache := cache.NewBuffer(
		cfg.Cache.MaxRAMMB*1024*1024,    // bytes
		cfg.Cache.ChunkSizeMB*1024*1024, // bytes per chunk
		time.Duration(cfg.Cache.TTLSeconds)*time.Second,
		evictionStrategy,
	)
	defer ramCache.Stop()

	// --- Throttle queue ---
	throttleQueue := throttle.NewQueue(cfg.Throttle.RequestsPerMinute)

	// --- Context for graceful shutdown ---
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start the throttle processing loop.
	throttleQueue.Start(ctx)

	// --- Metadata sync worker ---
	syncWorker := metadata.NewSyncWorker(
		metadataStore,
		torBoxClient,
		throttleQueue,
		time.Duration(cfg.Sync.IntervalMinutes)*time.Minute,
		cfg.Sync.Limit,
	)
	go syncWorker.Start(ctx)

	// --- Action callbacks ---
	server.SetActions(
		// Resync: triggers an immediate metadata sync.
		func() error {
			syncWorker.SyncNow()
			return nil
		},
		// Clear cache: evicts all RAM cached chunks.
		func() error {
			ramCache.Clear()
			return nil
		},
	)

	// --- WebDAV server ---
	srv := server.New(
		server.Config{
			ListenAddr:         cfg.Server.ListenAddr,
			WebDAVRoot:         cfg.Server.WebDAVRoot,
			CDNTtlMinutes:      cfg.Cache.CDNURLTTLMinutes,
			Version:            Version,
			MaxRAMMB:           cfg.Cache.MaxRAMMB,
			ChunkSizeMB:        cfg.Cache.ChunkSizeMB,
			TTLSeconds:         cfg.Cache.TTLSeconds,
			EvictionStrategy:   cfg.Cache.EvictionStrategy,
			RequestsPerMinute:  cfg.Throttle.RequestsPerMinute,
			LogFormat:          cfg.Logging.Format,
			LogLevel:           cfg.Logging.Level,
			SyncIntervalMinute: cfg.Sync.IntervalMinutes,
		},
		metadataStore,
		ramCache,
		torBoxClient,
		throttleQueue,
	)

	// Start the server in a goroutine.
	serverErr := make(chan error, 1)
	go func() {
		if err := srv.Start(ctx); err != nil {
			serverErr <- err
		}
	}()

	slog.Info("warpbox ready", "version", Version)

	// Block until signal or server error.
	select {
	case <-ctx.Done():
		slog.Info("shutting down warpbox")
	case err := <-serverErr:
		slog.Error("server error", "error", err)
	}
}