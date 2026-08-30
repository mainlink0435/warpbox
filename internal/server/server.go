// Package server implements the WebDAV HTTP handler for Warpbox.
//
// It handles PROPFIND (directory listing), GET with Range (streaming),
// HEAD, and OPTIONS methods. All reads go through the throttle →
// metadata pipeline.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	_ "net/http/pprof"
	"log/slog"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mainlink0435/warpbox/internal/config"
	"github.com/mainlink0435/warpbox/internal/library"
	"github.com/mainlink0435/warpbox/internal/metadata"
	"github.com/mainlink0435/warpbox/internal/openapi"
	"github.com/mainlink0435/warpbox/internal/throttle"
	"github.com/mainlink0435/warpbox/internal/torbox"
)

// SyncStatusFunc is a callback that returns the current sync status.
type SyncStatusFunc func() metadata.SyncStatus

// negativeCacheEntry tracks a failed CDN URL fetch so we don't hammer TorBox
// on rapid retries from Plex/jellyfin. Entries expire after their TTL.
type negativeCacheEntry struct {
	err       error
	expiresAt time.Time
}

// failureKind classifies a CDN URL fetch failure so the circuit breaker can
// distinguish item-scoped failures (quarantine candidates) from transient or
// account-level ones (which must not be labelled "dead").
type failureKind string

const (
	failureKindAuth          failureKind = "auth"
	failureKindTransient     failureKind = "transient"
	failureKindDatabaseError failureKind = "database_error"
)

// torrentFailureTracker counts failures for a single torrent within a sliding
// window. Once the threshold is exceeded, the torrent is marked "stale" and
// all CDN URL fetches are skipped until the next metadata sync.
//
// IMPORTANT: trackers are keyed by item id only — never by hash or path — so
// that removing and re-adding a torrent at TorBox (which yields a new item id)
// produces a fresh tracker with no inherited quarantine.
type torrentFailureTracker struct {
	failures   []time.Time
	staleUntil time.Time

	// Details surfaced on the landing page "failed links" list.
	errKind            failureKind
	lastErr            string
	errorCode          string // e.g. "DATABASE_ERROR"
	trippedAt          time.Time
	escalations        int  // how many times the stale window has been doubled
	quarantineNotified bool // a "remove and re-add" warning has been logged
}

// QuarantinedItem is a snapshot of a circuit-breaker entry for the landing page.
type QuarantinedItem struct {
	ItemID      int64
	Label       string
	ErrKind     failureKind
	ErrorCode   string
	LastError   string
	Failures    int
	TrippedAt   time.Time
	StaleUntil  time.Time
	Escalations int
	Tripped     bool // true once the breaker threshold was reached (quarantined)
}

// requestdlOutcome records a single requestdl result for the global health
// tracker.
type requestdlOutcome struct {
	at time.Time
	ok bool
}

// Global requestdl health window and degraded thresholds. These are constants
// (not config) — they gate the quarantine escalation, not tuneable behaviour.
const (
	defaultGlobalHealthWindow = 5 * time.Minute // fallback when stats.api_health_window_seconds is unset
	globalHealthMinSamples    = 5
	globalHealthFailRatio     = 0.8
	breakerProbeInterval      = 15 * time.Second // single half-open probe window after a stale period
)

// healthWindow returns the configured API-health / flap-detection window,
// falling back to 5 minutes when unset (e.g. direct constructions/tests).
func (s *Server) healthWindow() time.Duration {
	w := time.Duration(s.cfg.APIHealthWindowSeconds) * time.Second
	if w <= 0 {
		w = defaultGlobalHealthWindow
	}
	return w
}

// Server is the Warpbox WebDAV server.
type Server struct {
	cfg        Config
	store      *metadata.Store
	torBox     *torbox.Client
	queue      *throttle.Queue
	root       string
	mux        *chi.Mux
	httpServer *http.Server
	startTime  time.Time
	syncStatus SyncStatusFunc

	// Negative cache: key = "torrentID:fileID", value = error + expiry.
	negativeCache   map[string]*negativeCacheEntry
	negativeCacheMu sync.Mutex

	// Circuit breaker: per-torrent failure tracking.
	torrentFailures   map[int64]*torrentFailureTracker
	torrentFailuresMu sync.Mutex

	// Global requestdl health tracker: rolling window of outcomes used to
	// detect a transient TorBox-wide outage so individual items aren't
	// falsely labelled "dead" while the API as a whole is failing.
	requestdlHealthMu sync.Mutex
	requestdlOutcomes []requestdlOutcome

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

	// Library virtual path filters and lookup map.
	virtualFilters   []*library.Filter
	virtualPathMap   map[string]*library.Filter // name → filter for O(1) lookup

	// TorBox user info (refreshed periodically).
	torboxUserInfo   *torbox.UserInfo
	torboxUserInfoMu sync.Mutex

	// CSRF token for management action endpoints.
	csrfToken string
}

// webdavRoot is the canonical WebDAV root path — always "/webdav".
const webdavRoot = "/webdav"

// Config holds the server-specific configuration.
type Config struct {
	ListenAddr         string
	CDNTtlMinutes       int  // How long to cache CDN URLs (0 = disable)
	CDNURLAutoRepair    bool // Auto-repair stale CDN URLs
	CDNURLRepairRetries int  // Max repair retries per request
	Version            string // Build version
	RequestsPerMinute  int    // For landing page display
	LogFormat          string // For landing page display
	LogLevel           string // For landing page display
	SyncIntervalMinute int    // For landing page display
	SyncListPageSize   int    // For landing page display

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
	CircuitBreakerMaxStaleMin int // Max escalated stale window minutes; default 60

	// Memory management settings.
	NegativeCacheMaxEntries  int // Max entries in negative cache; default 5000
	CircuitBreakerMaxEntries int // Max entries in circuit breaker; default 2000
	CleanupIntervalSeconds   int // How often to sweep expired entries; default 60

	// CDN proxy settings.
	MaxCDNConnections int // Max concurrent CDN proxy connections; default 4

	// CDN timeout settings (seconds).
	CDNProxyTimeoutSeconds  int // Timeout for proxying a CDN byte range; default 30
	CDNURL429BackoffSeconds int // Backoff for a 429 when fetching a CDN URL; default 30

	// Path to config file for runtime log level toggle.
	ConfigPath string

	// Stats collection settings.
	StatsIntervalSeconds int // How often to record stats; default 60
	StatsRetentionHours  int // How long to retain stats rows; default 24
	StatsChartMinutes    int // How far back the landing page chart shows; default 60
	APIHealthWindowSeconds int // Rolling window for the API-health indicator & flap detection; default 300

	// LevelVar for atomic runtime log level switching. Shared with main.go's
	// slog.HandlerOptions.Level so a Set() call takes effect immediately.
	// Must be set by main.go after New() returns.
	LevelVar *slog.LevelVar

	// Auth settings for optional HTTP Basic Authentication on web UI.
	AuthEnabled  bool
	AuthUsername string
	AuthPassword string

	// Library virtual path configuration.
	VirtualPaths []config.VirtualPathConfig
}

// New creates a new WebDAV server.
func New(cfg Config, store *metadata.Store, torBox *torbox.Client, queue *throttle.Queue) *Server {
	maxConns := cfg.MaxCDNConnections
	if maxConns < 1 {
		maxConns = 4
	}
	// Default CDN timeout settings so direct Config construction (tests) and
	// any caller that omits them keeps the historical 30s behaviour.
	if cfg.CDNProxyTimeoutSeconds <= 0 {
		cfg.CDNProxyTimeoutSeconds = 30
	}
	if cfg.CDNURL429BackoffSeconds <= 0 {
		cfg.CDNURL429BackoffSeconds = 30
	}

	virtualFilters, vpErr := buildFilters(cfg.VirtualPaths)
	if vpErr != nil {
		slog.Error("server: failed to build virtual path filters", "error", vpErr)
	}

	csrfToken := generateCSRFToken()

	s := &Server{
		cfg:       cfg,
		store:     store,
		torBox:    torBox,
		queue:     queue,
		root:      webdavRoot,
		mux:       chi.NewRouter(),
		startTime: time.Now(),
		csrfToken: csrfToken,

		negativeCache:          make(map[string]*negativeCacheEntry),
		torrentFailures:        make(map[int64]*torrentFailureTracker),
		cleanupStopCh:          make(chan struct{}),
		cdnSem:                 make(chan struct{}, maxConns),
		negativeCacheMaxEntries:  cfg.NegativeCacheMaxEntries,
		circuitBreakerMaxEntries: cfg.CircuitBreakerMaxEntries,
		configPath:             cfg.ConfigPath,
		statsRetention:          time.Duration(cfg.StatsRetentionHours) * time.Hour,
		statsChartSince:         time.Duration(cfg.StatsChartMinutes) * time.Minute,
		virtualFilters:          virtualFilters,
		virtualPathMap:          makeVirtualPathMap(virtualFilters),
	}
	// Fill the semaphore so we can Acquire/Release.
	for i := 0; i < maxConns; i++ {
		s.cdnSem <- struct{}{}
	}
	s.registerRoutes()
	s.startCleanupLoop()
	return s
}

func buildFilters(vps []config.VirtualPathConfig) ([]*library.Filter, error) {
	filters := make([]*library.Filter, 0, len(vps))
	for _, vp := range vps {
		f, err := library.NewFilter("/"+vp.Name, vp.DirectoryInclude, vp.DirectoryExclude, vp.FileRegex, vp.LargestFileOnly)
		if err != nil {
			return nil, fmt.Errorf("building filter for %q: %w", vp.Name, err)
		}
		// Size bounds validated at config load; re-parse for filter (0 = unlimited).
		minSize, err := library.ParseFileSize(vp.MinFileSize)
		if err != nil {
			return nil, fmt.Errorf("building filter for %q: min_file_size: %w", vp.Name, err)
		}
		maxSize, err := library.ParseFileSize(vp.MaxFileSize)
		if err != nil {
			return nil, fmt.Errorf("building filter for %q: max_file_size: %w", vp.Name, err)
		}
		f.MinSize = minSize
		f.MaxSize = maxSize
		filters = append(filters, f)
	}
	return filters, nil
}

func makeVirtualPathMap(filters []*library.Filter) map[string]*library.Filter {
	m := make(map[string]*library.Filter, len(filters))
	for _, f := range filters {
		m[strings.TrimPrefix(f.Mount, "/")] = f
	}
	return m
}

// generateCSRFToken returns a hex-encoded 32-byte random token.
func generateCSRFToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("failed to generate CSRF token: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// csrfMiddleware checks that POST requests carry a valid CSRF token.
func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.Header.Get("X-CSRF-Token") != s.csrfToken {
				http.Error(w, "invalid or missing CSRF token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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

// startCleanupLoop runs periodic background goroutines that sweep expired
// entries from the negative cache and circuit breaker maps, and records
// time-series stats. Cleanup and stats run on independent tickers so they
// can be configured at different intervals.
func (s *Server) startCleanupLoop() {
	cleanupInterval := time.Duration(s.cfg.CleanupIntervalSeconds) * time.Second
	if cleanupInterval <= 0 {
		cleanupInterval = 60 * time.Second
	}
	statsInterval := time.Duration(s.cfg.StatsIntervalSeconds) * time.Second
	if statsInterval <= 0 {
		statsInterval = 60 * time.Second
	}
	go func() {
		cleanupTick := time.NewTicker(cleanupInterval)
		statsTick := time.NewTicker(statsInterval)
		defer cleanupTick.Stop()
		defer statsTick.Stop()
		for {
			select {
			case <-cleanupTick.C:
				s.sweepNegativeCache()
				s.sweepCircuitBreaker()
			case <-statsTick.C:
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

	// Requestdl API health as a 0-100 gauge over the trailing window, so charts
	// show how flaky the API is over time. Skipped when the window has no samples.
	if h := s.requestdlHealth(); h.Samples > 0 {
		metrics["requestdl_success_ratio"] = h.Ratio * 100
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

// Context keys for passing library filter and mount root through context.
type filterKeyType struct{}
type mountRootKeyType struct{}

var filterKey filterKeyType
var mountRootKey mountRootKeyType

// rootForRequest returns the WebDAV root path to use for prefix stripping.
// When inside a virtual path (e.g., /webdav/movies/), returns the full mount
// path. Falls back to s.root for the unfiltered /webdav/ view.
func (s *Server) rootForRequest(r *http.Request) string {
	if mount, ok := r.Context().Value(mountRootKey).(string); ok && mount != "" {
		return mount
	}
	return s.root
}

// virtualPathName returns the first path segment after /webdav/ or empty string.
// e.g. "/webdav/movies/file" → "movies", "/webdav/" → ""
func virtualPathName(path string) string {
	p := strings.TrimPrefix(path, webdavRoot)
	p = strings.TrimPrefix(p, "/")
	if idx := strings.IndexByte(p, '/'); idx >= 0 {
		return p[:idx]
	}
	return p
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
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].expiresAt.Before(sorted[j].expiresAt)
		})
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
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].staleUntil.Before(sorted[j].staleUntil)
		})
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

// recordOutcome records a single TorBox API/CDN outcome for the global health
// tracker, pruning outcomes older than the window. Fed by every HTTP attempt
// (sync mylist, requestdl, user/me, and CDN data-plane calls) so retried-and-
// recovered failures still count, rather than being invisible.
func (s *Server) recordOutcome(ok bool) {
	s.requestdlHealthMu.Lock()
	defer s.requestdlHealthMu.Unlock()

	now := time.Now()
	s.requestdlOutcomes = append(s.requestdlOutcomes, requestdlOutcome{at: now, ok: ok})

	cutoff := now.Add(-s.healthWindow())
	keep := s.requestdlOutcomes[:0]
	for _, o := range s.requestdlOutcomes {
		if o.at.After(cutoff) {
			keep = append(keep, o)
		}
	}
	s.requestdlOutcomes = keep
}

// RecordTorBoxOutcome feeds any TorBox HTTP outcome into the global health
// tracker. It is wired to torbox.Client.OnOutcome so every API attempt is
// counted at the raw HTTP level, including attempts that later recover on a
// retry. endpoint and status are accepted for forward compatibility (only the
// success/failure bit is tracked today).
func (s *Server) RecordTorBoxOutcome(endpoint string, status int, ok bool) {
	s.recordOutcome(ok)
}

// globalDegraded reports whether the requestdl API as a whole looks degraded
// (e.g. a transient TorBox outage). When true, item-scoped failures are treated
// as transient and never escalated or labelled "dead", so a global flap cannot
// falsely quarantine healthy items.
func (s *Server) globalDegraded() bool {
	s.requestdlHealthMu.Lock()
	defer s.requestdlHealthMu.Unlock()

	now := time.Now()
	cutoff := now.Add(-s.healthWindow())
	var fails, succ int
	for _, o := range s.requestdlOutcomes {
		if o.at.After(cutoff) {
			if o.ok {
				succ++
			} else {
				fails++
			}
		}
	}
	if fails+succ < globalHealthMinSamples {
		return false
	}
	if succ == 0 {
		return true
	}
	return float64(fails)/float64(fails+succ) >= globalHealthFailRatio
}

// APIHealth is a snapshot of the requestdl API health over the rolling window,
// surfaced on the landing page so it is obvious when TorBox is flaky.
type APIHealth struct {
	Samples     int
	Successes   int
	Failures    int
	Ratio       float64 // successes / samples (0.0–1.0)
	LastSuccess time.Time
	LastFailure time.Time
	Degraded    bool // globalDegraded() over the window
	TooFew      bool // fewer than globalHealthMinSamples samples in the window
}

// requestdlHealth returns the current requestdl health over the rolling window.
func (s *Server) requestdlHealth() APIHealth {
	s.requestdlHealthMu.Lock()
	defer s.requestdlHealthMu.Unlock()

	now := time.Now()
	cutoff := now.Add(-s.healthWindow())
	h := APIHealth{}
	var fails, succ int
	var lastSucc, lastFail time.Time
	for _, o := range s.requestdlOutcomes {
		if !o.at.After(cutoff) {
			continue
		}
		h.Samples++
		if o.ok {
			succ++
			if o.at.After(lastSucc) {
				lastSucc = o.at
			}
		} else {
			fails++
			if o.at.After(lastFail) {
				lastFail = o.at
			}
		}
	}
	h.Successes = succ
	h.Failures = fails
	if h.Samples > 0 {
		h.Ratio = float64(succ) / float64(h.Samples)
	}
	h.LastSuccess = lastSucc
	h.LastFailure = lastFail
	h.TooFew = h.Samples < globalHealthMinSamples
	if !h.TooFew {
		if succ == 0 {
			h.Degraded = true
		} else {
			h.Degraded = float64(fails)/float64(h.Samples) >= globalHealthFailRatio
		}
	}
	return h
}

// Percent formats the success ratio as a percentage string for the landing page.
func (h APIHealth) Percent() string {
	if h.Samples == 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", h.Ratio*100)
}

// LastSuccessDisplay renders the last successful requestdl time with date,
// timezone, and relative context (e.g. "Aug 10 02:39:49 UTC (3m25s ago)").
func (h APIHealth) LastSuccessDisplay() string {
	return relativeTimeDisplay(h.LastSuccess)
}

// LastFailureDisplay renders the last failed requestdl time with date,
// timezone, and relative context.
func (h APIHealth) LastFailureDisplay() string {
	return relativeTimeDisplay(h.LastFailure)
}

// relativeTimeDisplay formats a time for the landing page, returning "—" for a
// zero time and otherwise "Jan 2 15:04:05 MST (X ago)".
func relativeTimeDisplay(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return fmt.Sprintf("%s (%s ago)", t.Format("Jan 2 15:04:05 MST"), formatDuration(time.Since(t)))
}

// clearTorrentFailure removes a circuit-breaker tracker after a successful
// CDN URL fetch, so a recovered item stops being quarantined immediately.
func (s *Server) clearTorrentFailure(itemID int64) {
	s.torrentFailuresMu.Lock()
	delete(s.torrentFailures, itemID)
	s.torrentFailuresMu.Unlock()
}

// QuarantinedItems returns a snapshot of the circuit-breaker entries for the
// landing page "failed links" section. Labels are resolved from the store so a
// re-added item shows its current name.
func (s *Server) QuarantinedItems() []QuarantinedItem {
	type raw struct {
		id int64
		t  *torrentFailureTracker
	}
	s.torrentFailuresMu.Lock()
	raws := make([]raw, 0, len(s.torrentFailures))
	for id, t := range s.torrentFailures {
		// List every tracked item — tripped (quarantined) and still-failing
		// (untripped) — so problems are visible immediately.
		raws = append(raws, raw{id, t})
	}
	s.torrentFailuresMu.Unlock()

	items := make([]QuarantinedItem, 0, len(raws))
	for _, r := range raws {
		label := ""
		if lbl, err := s.store.GetFileLabelByItemID(r.id); err == nil && lbl != "" {
			label = lbl
		} else {
			label = fmt.Sprintf("item #%d", r.id)
		}
		items = append(items, QuarantinedItem{
			ItemID:      r.id,
			Label:       label,
			ErrKind:     r.t.errKind,
			ErrorCode:   r.t.errorCode,
			LastError:   r.t.lastErr,
			Failures:    len(r.t.failures),
			TrippedAt:   r.t.trippedAt,
			StaleUntil:  r.t.staleUntil,
			Escalations: r.t.escalations,
			Tripped:     !r.t.trippedAt.IsZero(),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].StaleUntil.Before(items[j].StaleUntil)
	})
	return items
}

// PruneBreakerForMissingItems drops circuit-breaker trackers (and matching
// negative-cache entries) whose item id is no longer present in the store.
// Called at the end of each successful metadata sync so that removed/re-added
// items are not stuck in quarantine for a full stale window. Quarantine is
// keyed by item id only, so a re-added torrent (new id) never inherits the old
// item's dead state.
func (s *Server) PruneBreakerForMissingItems(liveIDs map[int64]struct{}) {
	s.torrentFailuresMu.Lock()
	for id := range s.torrentFailures {
		if _, ok := liveIDs[id]; !ok {
			delete(s.torrentFailures, id)
		}
	}
	s.torrentFailuresMu.Unlock()

	s.negativeCacheMu.Lock()
	for k := range s.negativeCache {
		// Negative-cache keys are "src:itemID:fileID".
		parts := strings.Split(k, ":")
		if len(parts) == 3 {
			if id, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
				if _, ok := liveIDs[id]; !ok {
					delete(s.negativeCache, k)
				}
			}
		}
	}
	s.negativeCacheMu.Unlock()
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

	// Detect virtual path mounts from the first path segment.
	vpName := virtualPathName(r.URL.Path)
	var cfilter *library.Filter
	var croot string
	if vpName == "__all__" {
		// Unfiltered view — strip __all__ from the path root.
		croot = s.root + "/__all__"
	} else if f, ok := s.virtualPathMap[vpName]; ok {
		// Registered virtual path — apply filter and set root to mount path.
		cfilter = f
		croot = s.root + "/" + vpName
	} else if len(s.virtualFilters) > 0 && vpName == "" {
		// Root of /webdav/ with virtual paths configured — show synthetic dirs.
		// No context values needed; handlePropfind/serveDirListing detect this.
	}

	if croot != "" || cfilter != nil {
		ctx := r.Context()
		if croot != "" {
			ctx = context.WithValue(ctx, mountRootKey, croot)
		}
		if cfilter != nil {
			ctx = context.WithValue(ctx, filterKey, cfilter)
		}
		r = r.WithContext(ctx)
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

	// requireAuth adapts the main branch's Basic Auth middleware for chi.
	// When auth is disabled it passes through without checking.
	requireAuth := func(next http.Handler) http.Handler {
		return s.requireAuth(next.ServeHTTP)
	}

	// WebDAV routes — internal dispatch handles GET, HEAD, OPTIONS, PROPFIND.
	// Chi's Handle is used (not per-method) because PROPFIND is non-standard.
	s.mux.Handle(s.root+"/*", http.HandlerFunc(s.handleWebDAV))
	s.mux.Handle(s.root, http.HandlerFunc(s.handleWebDAV))
	s.mux.Handle("/infuse/*", http.HandlerFunc(s.handleWebDAV))
	s.mux.Handle("/infuse", http.HandlerFunc(s.handleWebDAV))

	// HTTP browser (directory listing + file streaming with CDN proxy).
	s.mux.With(requireAuth).Get("/http/*", s.handleHTTP)
	s.mux.With(requireAuth).Get("/http", s.handleHTTP)

	// Logs endpoint.
	s.mux.With(requireAuth).Get("/logs", s.handleLogs)
	s.mux.With(requireAuth).Get("/logs/", s.handleLogs)

	// API: Health check (no auth — used by uptime monitors / load balancers).
	s.mux.Method("GET", "/healthz", openapi.Annotated(
		http.HandlerFunc(s.handleHealthz),
		openapi.Operation{
			Summary:     "Health check",
			Description: "Returns the health status of the warpbox server and its database connection.",
			Tags:        []string{"System"},
			Responses: map[string]openapi.Response{
				"200": {
					Description: "Server is healthy",
					Content: openapi.JSONContent(openapi.Schema{
						Type: "object",
						Properties: map[string]*openapi.Schema{
							"status": {Type: "string", Example: "ok"},
						},
					}),
				},
				"503": {
					Description: "Server is unhealthy (e.g. database closed)",
					Content: openapi.JSONContent(openapi.Schema{
						Type: "object",
						Properties: map[string]*openapi.Schema{
							"status": {Type: "string", Example: "error"},
							"detail": {Type: "string", Example: "database unreachable"},
						},
					}),
				},
			},
		},
	))

	// API: Time-series stats (auth required).
	s.mux.With(requireAuth).Method("GET", "/stats.json", openapi.Annotated(
		http.HandlerFunc(s.handleStatsJSON),
		openapi.Operation{
			Summary:     "Time-series metrics",
			Description: "Returns recorded metrics (API calls, memory, cache sizes) for the configured time window.",
			Tags:        []string{"Monitoring"},
			Responses: map[string]openapi.Response{
				"200": {
					Description: "Metric data keyed by metric name",
					Content: openapi.JSONContent(openapi.Schema{
						Type: "object",
						AdditionalProperties: &openapi.Schema{
							Type: "array",
							Items: &openapi.Schema{
								Type: "object",
								Properties: map[string]*openapi.Schema{
									"t": {Type: "string", Format: "date-time", Example: "2026-01-15T10:30:00Z"},
									"v": {Type: "number", Example: 42},
								},
							},
						},
					}),
				},
				"500": {Description: "Internal error"},
			},
		},
	))

	// API: Actions (management sub-routes, all require auth).
	s.mux.Route("/actions", func(r chi.Router) {
		r.Use(requireAuth)
		r.Use(s.csrfMiddleware)
		r.Method("POST", "/resync", openapi.Annotated(
			http.HandlerFunc(s.handleResync),
			openapi.Operation{
				Summary:     "Trigger metadata resync",
				Description: "Triggers an immediate metadata sync from TorBox. The sync runs asynchronously.",
				Tags:        []string{"Management"},
				Responses: map[string]openapi.Response{
					"200": {Description: "Resync triggered successfully"},
					"500": {Description: "Resync action not configured"},
				},
			},
		))

		r.Method("POST", "/restart-sync", openapi.Annotated(
			http.HandlerFunc(s.handleRestartSync),
			openapi.Operation{
				Summary:     "Restart sync worker",
				Description: "Stops the current sync worker loop and starts a fresh one.",
				Tags:        []string{"Management"},
				Responses: map[string]openapi.Response{
					"200": {Description: "Sync worker restart triggered"},
					"500": {Description: "Restart action not configured"},
				},
			},
		))

		r.Method("POST", "/loglevel", openapi.Annotated(
			http.HandlerFunc(s.handleLogLevel),
			openapi.Operation{
				Summary:     "Change log level",
				Description: "Changes the runtime log level and persists it to the config file.",
				Tags:        []string{"Management"},
				RequestBody: &openapi.RequestBody{
					Required: true,
					Content: openapi.FormContent(openapi.Schema{
						Type: "object",
						Properties: map[string]*openapi.Schema{
							"level": {
								Type:    "string",
								Enum:    []string{"debug", "info", "warn", "error"},
								Example: "debug",
							},
						},
						Required: []string{"level"},
					}),
				},
				Responses: map[string]openapi.Response{
					"200": {Description: "Log level changed"},
					"400": {Description: "Missing or invalid level parameter"},
					"500": {Description: "Failed to persist log level"},
				},
			},
		))
	})

	// pprof endpoints (conditional).
	if s.cfg.EnablePprof {
		s.mux.With(requireAuth).HandleFunc("/debug/pprof", http.DefaultServeMux.ServeHTTP)
		s.mux.With(requireAuth).HandleFunc("/debug/pprof/*", http.DefaultServeMux.ServeHTTP)
	}

	// Landing page (exact match only).
	s.mux.With(requireAuth).Get("/", s.handleLanding)

	// Static assets.
	s.mux.Get("/chart.umd.min.js", s.handleChartJS)
	s.mux.Get("/warpbox.svg", s.handleLogo)
	s.mux.Get("/favicon.ico", s.handleLogo)

	// OpenAPI spec — build from annotated routes, then serve at /openapi.json.
	// The spec is built after all routes are registered so Walk enumerates
	// the complete route tree. /openapi.json is registered last so it does
	// not appear in its own spec output.
	spec := openapi.NewBuilder(openapi.Info{
		Title:       "Warpbox API",
		Description: "Management and monitoring endpoints for the Warpbox media server proxy.",
		Version:     s.cfg.Version,
	})
	spec.BuildFromRouter(s.mux)
	s.mux.With(requireAuth).Get("/openapi.json", spec.Handler())
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
	s.httpServer = &http.Server{Addr: s.cfg.ListenAddr, Handler: s.mux}
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the HTTP server with a timeout context.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

