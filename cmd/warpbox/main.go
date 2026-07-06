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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mainlink0435/warpbox/internal/config"
	"github.com/mainlink0435/warpbox/internal/metadata"
	"github.com/mainlink0435/warpbox/internal/server"
	"github.com/mainlink0435/warpbox/internal/throttle"
	"github.com/mainlink0435/warpbox/internal/torbox"
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

	// --- Structured logging with dynamic level switching ---
	logLevel, err := config.ParseLevel(cfg.Logging.Level)
	if err != nil {
		slog.Error("invalid logging level", "level", cfg.Logging.Level, "error", err)
		os.Exit(1)
	}

	// Use a LevelVar for atomic runtime log level switching.
	levelVar := &slog.LevelVar{}
	levelVar.Set(logLevel)

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: levelVar}
	if cfg.Logging.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	bufHandler := server.NewRingBufferHandler(handler)
	server.SetLogBuffer(bufHandler)
	logger := slog.New(bufHandler)
	slog.SetDefault(logger)
	if cfg.Logging.Level != "info" {
		slog.Warn("log level is not info — this may affect performance in production", "level", cfg.Logging.Level)
	}

	fmt.Print(banner)
	fmt.Printf("\nwarpbox %s — WebDAV proxy for TorBox\n\n", Version)

	slog.Info("starting warpbox",
		"version", Version,
		"listen_addr", cfg.Server.ListenAddr,
		"log_format", cfg.Logging.Format,
		"log_level", cfg.Logging.Level,
	)

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

	throttleQueue := throttle.NewQueue(cfg.Throttle.RequestsPerMinute)

	torBoxClient := torbox.NewClient(cfg.TorBox.APIKey)
	torBoxClient.HTTP429Callback = func() { throttleQueue.Record429() }

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	throttleQueue.Start(ctx)

	syncWorker := metadata.NewSyncWorker(
		metadataStore,
		torBoxClient,
		throttleQueue,
		time.Duration(cfg.Sync.IntervalMinutes)*time.Minute,
		cfg.Sync.ListPageSize,
		cfg.Sync.BypassCache,
		*cfg.Sync.RetryAttempts,
		time.Duration(*cfg.Sync.RetryBackoff)*time.Second,
	)

	// Set library change hooks.
	libCfg := &cfg.Library
	if libCfg.OnItemsAdded != "" {
		syncWorker.OnItemsAdded = func(items []string) {
			runItemsHook(libCfg.OnItemsAdded, libCfg.HookTimeoutSec, items)
		}
	}
	if libCfg.OnItemsRemoved != "" {
		syncWorker.OnItemsRemoved = func(items []string) {
			runItemsHook(libCfg.OnItemsRemoved, libCfg.HookTimeoutSec, items)
		}
	}

	go syncWorker.Start(ctx)

	server.SetActions(map[string]server.ActionFunc{
		"resync": func() error {
			syncWorker.SyncNow()
			return nil
		},
		"restart-sync": func() error {
			syncWorker.Restart()
			return nil
		},
	})

	// Wire the LevelVar into the server config for runtime log level toggle.
	serverCfg := server.Config{
		ListenAddr:              cfg.Server.ListenAddr,
		CDNTtlMinutes:           cfg.Cache.CDNURLTTLMinutes,
		CDNURLAutoRepair:        *cfg.Cache.CDNURLAutoRepair,
		CDNURLRepairRetries:     *cfg.Cache.CDNURLRepairRetries,
		Version:                 Version,
		RequestsPerMinute:       cfg.Throttle.RequestsPerMinute,
		LogFormat:               cfg.Logging.Format,
		LogLevel:                cfg.Logging.Level,
		SyncIntervalMinute:      cfg.Sync.IntervalMinutes,
		SyncListPageSize:        cfg.Sync.ListPageSize,
		EnablePprof:             cfg.Server.EnablePprof,
		CDNURLRetryBackoff:      *cfg.Cache.CDNURLRetryBackoff,
		CDNURLRetryCount:        *cfg.Cache.CDNURLRetryCount,
		NegativeCacheTTLSeconds: *cfg.Cache.NegativeCacheTTLSeconds,
		CircuitBreakerFailures:  *cfg.Cache.CircuitBreakerFailures,
		CircuitBreakerWindowSec: *cfg.Cache.CircuitBreakerWindowSec,
		CircuitBreakerStaleMin:  *cfg.Cache.CircuitBreakerStaleMin,
		NegativeCacheMaxEntries:  *cfg.Cache.NegativeCacheMaxEntries,
		CircuitBreakerMaxEntries: *cfg.Cache.CircuitBreakerMaxEntries,
		CleanupIntervalSeconds:  *cfg.Cache.CleanupIntervalSeconds,
		MaxCDNConnections:       *cfg.Cache.MaxCDNConnections,
		ConfigPath:              *configPath,
		StatsIntervalSeconds:    cfg.Stats.IntervalSeconds,
		StatsRetentionHours:     cfg.Stats.RetentionHours,
		StatsChartMinutes:       cfg.Stats.ChartMinutes,
	}
	serverCfg.LevelVar = levelVar

	serverCfg.AuthEnabled = cfg.Auth.Enabled
	serverCfg.AuthUsername = cfg.Auth.Username
	serverCfg.AuthPassword = cfg.Auth.Password
	serverCfg.VirtualPaths = cfg.Library.VirtualPaths

	srv := server.New(
		serverCfg,
		metadataStore,
		torBoxClient,
		throttleQueue,
	)
	srv.SetSyncStatus(syncWorker.Status)

	serverErr := make(chan error, 1)
	go func() {
		if err := srv.Start(ctx); err != nil {
			serverErr <- err
		}
	}()

	// Fetch TorBox user info at startup.
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if ui, err := torBoxClient.GetUserInfo(ctx); err != nil {
			slog.Warn("failed to fetch TorBox user info", "error", err)
		} else {
			srv.SetTorBoxUserInfo(ui)
		}
	}()

	slog.Info("warpbox ready", "version", Version)

	select {
	case <-ctx.Done():
		slog.Info("shutting down warpbox")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("server shutdown error", "error", err)
		}
	case err := <-serverErr:
		slog.Error("server error", "error", err)
	}
}

func runItemsHook(command string, timeoutSec int, items []string) {
	if command == "" || len(items) == 0 {
		return
	}
	parts := strings.Fields(command)
	if len(parts) == 0 {
		slog.Warn("empty hook command")
		return
	}
	args := append(parts[1:], items...)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, parts[0], args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			slog.Warn("hook timed out", "command", command, "timeout_seconds", timeoutSec, "items", items)
		} else {
			slog.Error("hook failed", "command", command, "error", err, "output", string(output), "items", items)
		}
		return
	}
	if len(output) > 0 {
		slog.Info("hook output", "command", command, "output", string(output), "items", items)
	}
}