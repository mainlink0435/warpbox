// Package server implements the WebDAV HTTP handler for Warpbox.
//
// It handles PROPFIND (directory listing), GET with Range (streaming),
// HEAD, and OPTIONS methods. All reads go through the throttle →
// metadata pipeline.
package server

import (
	"context"
	"encoding/json"
	_ "net/http/pprof"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ben/warpbox/internal/metadata"
	"github.com/ben/warpbox/internal/throttle"
	"github.com/ben/warpbox/internal/torbox"
)

// SyncStatusFunc is a callback that returns the current sync status.
type SyncStatusFunc func() metadata.SyncStatus

// negativeCacheEntry tracks a failed CDN URL fetch so we don't hammer TorBox
// on rapid retries from Plex/jellyfin. Entries expire after their TTL.
type negativeCacheEntry struct {
	err       error
	expiresAt time.Time
}

// torrentFailureTracker counts failures for a single torrent within a sliding
// window. Once the threshold is exceeded, the torrent is marked "stale" and
// all CDN URL fetches are skipped until the next metadata sync.
type torrentFailureTracker struct {
	failures   []time.Time
	staleUntil time.Time
}

// Server is the Warpbox WebDAV server.
type Server struct {
	cfg        Config
	store      *metadata.Store
	torBox     *torbox.Client
	queue      *throttle.Queue
	root       string
	mux        *chi.Mux
	startTime  time.Time
	syncStatus SyncStatusFunc

	// Negative cache: key = "torrentID:fileID", value = error + expiry.
	negativeCache   map[string]*negativeCacheEntry
	negativeCacheMu sync.Mutex

	// Circuit breaker: per-torrent failure tracking.
	torrentFailures   map[int64]*torrentFailureTracker
	torrentFailuresMu sync.Mutex

	// CDN connection semaphore: limits concurrent proxy connections to TorBox CDN.
	cdnSem chan struct{}

	// Stop channel for periodic cleanup goroutines.
	cleanupStopCh chan struct{}

	// Configurable map size limits.
	negativeCacheMaxEntries  int
	circuitBreakerMaxEntries int

	// Path to config file for runtime log level toggle.
	configPath string

	// Stats recording config.
	statsRetention  time.Duration // How long to retain stats rows
	statsChartSince time.Duration // How far back the landing page chart shows

	// Previous counter values for computing per-interval deltas in recordStats().
	prevSuccessfulCalls int64
	prevFailedCalls     int64
	prev429Calls        int64
	prevDBLockErrors    int64
	prevNumGC           uint32

	// TorBox user info (refreshed periodically).
	torboxUserInfo   *torbox.UserInfo
	torboxUserInfoMu sync.Mutex
}

// Config holds the server-specific configuration.
type Config struct {
	ListenAddr         string
	WebDAVRoot         string
	CDNTtlMinutes       int  // How long to cache CDN URLs (0 = disable)
	CDNURLAutoRepair    bool // Auto-repair stale CDN URLs
	CDNURLRepairRetries int  // Max repair retries per request
	Version            string // Build version
	RequestsPerMinute  int    // For landing page display
	LogFormat          string // For landing page display
	LogLevel           string // For landing page display
	SyncIntervalMinute int    // For landing page display
	SyncLimit          int    // For landing page display

	// Pprof control.
	EnablePprof bool // Enable /debug/pprof/ endpoints; default false

	// CDN URL fetch retry settings.
	CDNURLRetryBackoff int // Backoff base in seconds; default 1
	CDNURLRetryCount   int // Max retry attempts; default 1

	// Negative cache TTL in seconds.
	NegativeCacheTTLSeconds int // default 30

	// Circuit breaker settings.
	CircuitBreakerFailures  int // Max failures in window; default 5
	CircuitBreakerWindowSec int // Sliding window seconds; default 60
	CircuitBreakerStaleMin  int // Stale duration minutes; default 5

	// Memory management settings.
	NegativeCacheMaxEntries  int // Max entries in negative cache; default 5000
	CircuitBreakerMaxEntries int // Max entries in circuit breaker; default 2000
	CleanupIntervalSeconds   int // How often to sweep expired entries; default 60

	// CDN proxy settings.
	MaxCDNConnections int // Max concurrent CDN proxy connections; default 4

	// Path to config file for runtime log level toggle.
	ConfigPath string

	// Stats collection settings.
	StatsIntervalSeconds int // How often to record stats; default 60
	StatsRetentionHours  int // How long to retain stats rows; default 24
	StatsChartMinutes    int // How far back the landing page chart shows; default 60

	// LevelVar for atomic runtime log level switching. Shared with main.go's
	// slog.HandlerOptions.Level so a Set() call takes effect immediately.
	// Must be set by main.go after New() returns.
	LevelVar *slog.LevelVar
}

// New creates a new WebDAV server.
func New(cfg Config, store *metadata.Store, torBox *torbox.Client, queue *throttle.Queue) *Server {
	maxConns := cfg.MaxCDNConnections
	if maxConns < 1 {
		maxConns = 4
	}

	s := &Server{
		cfg:       cfg,
		store:     store,
		torBox:    torBox,
		queue:     queue,
		root:      cfg.WebDAVRoot,
		mux:       chi.NewRouter(),
		startTime: time.Now(),

		negativeCache:          make(map[string]*negativeCacheEntry),
		torrentFailures:        make(map[int64]*torrentFailureTracker),
		cleanupStopCh:          make(chan struct{}),
		cdnSem:                 make(chan struct{}, maxConns),
		negativeCacheMaxEntries:  cfg.NegativeCacheMaxEntries,
		circuitBreakerMaxEntries: cfg.CircuitBreakerMaxEntries,
		configPath:             cfg.ConfigPath,
		statsRetention:          time.Duration(cfg.StatsRetentionHours) * time.Hour,
		statsChartSince:         time.Duration(cfg.StatsChartMinutes) * time.Minute,
	}
	// Fill the semaphore so we can Acquire/Release.
	for i := 0; i < maxConns; i++ {
		s.cdnSem <- struct{}{}
	}
	s.registerRoutes()
	s.startCleanupLoop()
	return s
}

// AcquireCDNConn blocks until a CDN connection slot is available.
func (s *Server) AcquireCDNConn() {
	<-s.cdnSem
}

// ReleaseCDNConn returns a CDN connection slot.
func (s *Server) ReleaseCDNConn() {
	s.cdnSem <- struct{}{}
}

// ConfigPath returns the path to the config file for runtime log level toggle.
func (s *Server) ConfigPath() string {
	return s.configPath
}

// startCleanupLoop runs a periodic background goroutine that sweeps expired
// entries from the negative cache and circuit breaker maps, and records
// time-series stats.
func (s *Server) startCleanupLoop() {
	interval := time.Duration(s.cfg.CleanupIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 60 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.sweepNegativeCache()
				s.sweepCircuitBreaker()
				s.recordStats()
				if s.statsRetention > 0 {
					if n, err := s.store.PruneStats(s.statsRetention); err != nil {
						slog.Debug("stats prune failed", "error", err)
					} else if n > 0 {
						slog.Debug("pruned old stats", "rows", n)
					}
				}
			case <-s.cleanupStopCh:
				return
			}
		}
	}()
}

// recordStats snapshots current metrics and writes them to the stats table.
// Counter metrics (success/fail/429/db_lock_errors/gc_cycles) are recorded
// as per-interval deltas so charts show rate, not cumulative totals.
// Gauge metrics (sys_mb, alloc_mb, heap_objects, cache sizes) show
// point-in-time snapshots.
func (s *Server) recordStats() {
	throttleStats := s.queue.Stats()

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	// Compute per-interval deltas for counter metrics.
	dSuccess := throttleStats.SuccessfulCalls - s.prevSuccessfulCalls
	dFailed := throttleStats.FailedCalls - s.prevFailedCalls
	d429 := throttleStats.HTTP429Calls - s.prev429Calls
	lockErrors := s.store.DBLockErrors()
	dLockErrors := lockErrors - s.prevDBLockErrors
	dNumGC := mem.NumGC - s.prevNumGC

	// Update prev values for next interval.
	s.prevSuccessfulCalls = throttleStats.SuccessfulCalls
	s.prevFailedCalls = throttleStats.FailedCalls
	s.prev429Calls = throttleStats.HTTP429Calls
	s.prevDBLockErrors = lockErrors
	s.prevNumGC = mem.NumGC

	metrics := map[string]float64{
		// Per-interval deltas.
		"api_calls_success": float64(dSuccess),
		"api_calls_failed":  float64(dFailed),
		"api_calls_429":     float64(d429),
		"db_lock_errors":    float64(dLockErrors),
		"gc_cycles":         float64(dNumGC),

		// Gauges — point-in-time values, not deltas.
		"sys_mb":                  float64(mem.Sys / 1024 / 1024),
		"alloc_mb":                float64(mem.Alloc / 1024 / 1024),
		"heap_objects":            float64(mem.HeapObjects),
		"negative_cache_entries":  float64(s.NegativeCacheSize()),
		"circuit_breaker_entries": float64(s.CircuitBreakerSize()),
	}

	if err := s.store.RecordStats(metrics); err != nil {
		slog.Debug("stats record failed", "error", err)
	}
}

// SetTorBoxUserInfo atomically stores the user info for the landing page.
func (s *Server) SetTorBoxUserInfo(info *torbox.UserInfo) {
	s.torboxUserInfoMu.Lock()
	defer s.torboxUserInfoMu.Unlock()
	s.torboxUserInfo = info
}

// TorBoxUserInfo returns the cached TorBox account info (may be nil).
func (s *Server) TorBoxUserInfo() *torbox.UserInfo {
	s.torboxUserInfoMu.Lock()
	defer s.torboxUserInfoMu.Unlock()
	return s.torboxUserInfo
}

// StopCleanup stops the periodic cleanup goroutine. Intended for tests.
func (s *Server) StopCleanup() {
	close(s.cleanupStopCh)
}

// sweepNegativeCache removes expired entries. If the map exceeds
// maxNegativeCacheEntries, the oldest entries are also evicted.
func (s *Server) sweepNegativeCache() {
	s.negativeCacheMu.Lock()
	defer s.negativeCacheMu.Unlock()

	now := time.Now()
	for k, v := range s.negativeCache {
		if now.After(v.expiresAt) {
			delete(s.negativeCache, k)
		}
	}

	if len(s.negativeCache) > s.negativeCacheMaxEntries {
		over := len(s.negativeCache) - s.negativeCacheMaxEntries
		type kv struct {
			key       string
			expiresAt time.Time
		}
		sorted := make([]kv, 0, len(s.negativeCache))
		for k, v := range s.negativeCache {
			sorted = append(sorted, kv{key: k, expiresAt: v.expiresAt})
		}
		for i := 0; i < len(sorted) && i < over; i++ {
			oldest := i
			for j := i + 1; j < len(sorted); j++ {
				if sorted[j].expiresAt.Before(sorted[oldest].expiresAt) {
					oldest = j
				}
			}
			sorted[i], sorted[oldest] = sorted[oldest], sorted[i]
		}
		for i := 0; i < over; i++ {
			delete(s.negativeCache, sorted[i].key)
		}
		slog.Debug("swept negative cache",
			"remaining", len(s.negativeCache),
			"evicted", over,
			"max", s.negativeCacheMaxEntries,
		)
	}
}

// sweepCircuitBreaker removes trackers where the stale period has expired.
func (s *Server) sweepCircuitBreaker() {
	s.torrentFailuresMu.Lock()
	defer s.torrentFailuresMu.Unlock()

	now := time.Now()
	for k, v := range s.torrentFailures {
		if !v.staleUntil.IsZero() && now.After(v.staleUntil) {
			delete(s.torrentFailures, k)
		}
	}

	if len(s.torrentFailures) > s.circuitBreakerMaxEntries {
		over := len(s.torrentFailures) - s.circuitBreakerMaxEntries
		type kv struct {
			key        int64
			staleUntil time.Time
		}
		sorted := make([]kv, 0, len(s.torrentFailures))
		for k, v := range s.torrentFailures {
			sorted = append(sorted, kv{key: k, staleUntil: v.staleUntil})
		}
		for i := 0; i < len(sorted) && i < over; i++ {
			oldest := i
			for j := i + 1; j < len(sorted); j++ {
				if sorted[j].staleUntil.Before(sorted[oldest].staleUntil) {
					oldest = j
				}
			}
			sorted[i], sorted[oldest] = sorted[oldest], sorted[i]
		}
		for i := 0; i < over; i++ {
			delete(s.torrentFailures, sorted[i].key)
		}
		slog.Debug("swept circuit breaker",
			"remaining", len(s.torrentFailures),
			"evicted", over,
			"max", s.circuitBreakerMaxEntries,
		)
	}
}

// NegativeCacheSize returns the current number of entries in the negative cache.
func (s *Server) NegativeCacheSize() int {
	s.negativeCacheMu.Lock()
	defer s.negativeCacheMu.Unlock()
	return len(s.negativeCache)
}

// CircuitBreakerSize returns the current number of entries in the circuit breaker.
func (s *Server) CircuitBreakerSize() int {
	s.torrentFailuresMu.Lock()
	defer s.torrentFailuresMu.Unlock()
	return len(s.torrentFailures)
}

// versionHeader is HTTP middleware that sets the Server header.
func (s *Server) versionHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "warpbox/"+s.cfg.Version)
		next.ServeHTTP(w, r)
	})
}

// handleWebDAV dispatches WebDAV methods for both WebDAV and Infuse paths.
// Chi's Handle is used (not per-method routing) because PROPFIND is not
// a standard HTTP method. Internal dispatch handles GET, HEAD, OPTIONS,
// and PROPFIND explicitly.
func (s *Server) handleWebDAV(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/infuse") {
		r.URL.Path = strings.Replace(r.URL.Path, "/infuse", s.root, 1)
	}
	switch r.Method {
	case http.MethodGet:
		s.handleGet(w, r)
	case http.MethodHead:
		s.handleHead(w, r)
	case http.MethodOptions:
		s.handleOptions(w, r)
	case "PROPFIND":
		s.handlePropfind(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// registerRoutes sets up the chi router with all HTTP routes.
func (s *Server) registerRoutes() {
	s.mux.Use(s.versionHeader)

	// Register PROPFIND as a supported HTTP method so chi routes it
	// instead of rejecting it at the routing level.
	chi.RegisterMethod("PROPFIND")

	// WebDAV routes — internal dispatch handles GET, HEAD, OPTIONS, PROPFIND.
	// Chi's Handle is used (not per-method) because PROPFIND is non-standard.
	s.mux.Handle(s.root+"/*", http.HandlerFunc(s.handleWebDAV))
	s.mux.Handle(s.root, http.HandlerFunc(s.handleWebDAV))
	s.mux.Handle("/infuse/*", http.HandlerFunc(s.handleWebDAV))
	s.mux.Handle("/infuse", http.HandlerFunc(s.handleWebDAV))

	// HTTP browser (directory listing + file streaming with CDN proxy).
	s.mux.Get("/http/*", s.handleHTTP)
	s.mux.Get("/http", s.handleHTTP)

	// Logs endpoint.
	s.mux.Get("/logs", s.handleLogs)
	s.mux.Get("/logs/", s.handleLogs)

	// Actions — POST-only sub-routes.
	s.mux.Route("/actions", func(r chi.Router) {
		r.Post("/resync", s.handleResync)
		r.Post("/restart-sync", s.handleRestartSync)
		r.Post("/loglevel", s.handleLogLevel)
	})

	// pprof endpoints (conditional).
	if s.cfg.EnablePprof {
		s.mux.HandleFunc("/debug/pprof", http.DefaultServeMux.ServeHTTP)
		s.mux.HandleFunc("/debug/pprof/*", http.DefaultServeMux.ServeHTTP)
	}

	// Health / stats.
	s.mux.Get("/healthz", s.handleHealthz)
	s.mux.Get("/stats.json", s.handleStatsJSON)

	// Landing page (exact match only).
	s.mux.Get("/", s.handleLanding)

	// Static assets.
	s.mux.Get("/warpbox.svg", s.handleLogo)
	s.mux.Get("/favicon.ico", s.handleLogo)
}

// SetSyncStatus configures the callback for reading sync worker status.
func (s *Server) SetSyncStatus(fn SyncStatusFunc) {
	s.syncStatus = fn
}

// handleOptions responds with WebDAV capabilities.
func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("DAV", "1")
	w.Header().Set("Allow", "OPTIONS, GET, HEAD, PROPFIND")
	w.WriteHeader(http.StatusOK)
}

// statsDataPoint is a single timestamped value for the JSON endpoint.
type statsDataPoint struct {
	T string  `json:"t"`
	V float64 `json:"v"`
}

// handleStatsJSON returns stats data as JSON for the Chart.js frontend.
// Query param: minutes (default: from config, currently 60).
func (s *Server) handleStatsJSON(w http.ResponseWriter, r *http.Request) {
	mins := s.cfg.StatsChartMinutes
	if mins <= 0 {
		mins = 60
	}
	since := time.Now().Add(-time.Duration(mins) * time.Minute)

	grouped, err := s.store.QueryAllStatsSince(since)
	if err != nil {
		slog.Error("stats.json query failed", "error", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	result := make(map[string][]statsDataPoint, len(grouped))
	for metric, records := range grouped {
		points := make([]statsDataPoint, len(records))
		for i, rec := range records {
			points[i] = statsDataPoint{
				T: rec.Timestamp.UTC().Format(time.RFC3339),
				V: rec.Value,
			}
		}
		result[metric] = points
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		slog.Error("stats.json encode failed", "error", err)
	}
}

// Start begins listening on the configured address.
func (s *Server) Start(ctx context.Context) error {
	slog.Info("webdav server listening", "addr", s.cfg.ListenAddr)
	return http.ListenAndServe(s.cfg.ListenAddr, s.mux)
}

