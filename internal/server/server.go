// Package server implements the WebDAV HTTP handler for Warpbox.
//
// It handles PROPFIND (directory listing), GET with Range (streaming),
// HEAD, and OPTIONS methods. All reads go through the throttle → cache →
// metadata pipeline.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ben/warpbox/internal/cache"
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
	cache      *cache.Buffer
	torBox     *torbox.Client
	queue      *throttle.Queue
	root       string
	mux        *http.ServeMux
	startTime  time.Time
	syncStatus SyncStatusFunc

	// Negative cache: key = "torrentID:fileID", value = error + expiry.
	// Protects against Plex's tight retry loop burning API quota on known-bad files.
	negativeCache   map[string]*negativeCacheEntry
	negativeCacheMu sync.Mutex

	// Circuit breaker: per-torrent failure tracking.
	// Marked stale after maxTorrentFailures in the sliding window.
	torrentFailures   map[int64]*torrentFailureTracker
	torrentFailuresMu sync.Mutex
}

// Config holds the server-specific configuration.
type Config struct {
	ListenAddr         string
	WebDAVRoot         string
	CDNTtlMinutes       int  // How long to cache CDN URLs (0 = disable)
	CDNURLAutoRepair    bool // Auto-repair stale CDN URLs by re-fetching from TorBox
	CDNURLRepairRetries int  // Max repair retries per request (0 = no retries)
	Version            string // Build version, injected at compile time
	MaxRAMMB           int    // For landing page display
	ChunkSizeMB        int    // For landing page display
	TTLSeconds         int    // For landing page display
	EvictionStrategy   string // For landing page display
	RequestsPerMinute  int    // For landing page display
	LogFormat          string // For landing page display
	LogLevel           string // For landing page display
	SyncIntervalMinute int    // For landing page display

	// CDN URL fetch retry settings.
	CDNURLRetryBackoff int // Backoff base in seconds; default 1
	CDNURLRetryCount   int // Max retry attempts; default 3

	// Negative cache TTL in seconds.
	NegativeCacheTTLSeconds int // default 30

	// Circuit breaker settings.
	CircuitBreakerFailures  int // Max failures in window; default 5
	CircuitBreakerWindowSec int // Sliding window seconds; default 60
	CircuitBreakerStaleMin  int // Stale duration minutes; default 5
}

// New creates a new WebDAV server.
func New(cfg Config, store *metadata.Store, cache *cache.Buffer, torBox *torbox.Client, queue *throttle.Queue) *Server {
	s := &Server{
		cfg:       cfg,
		store:     store,
		cache:     cache,
		torBox:    torBox,
		queue:     queue,
		root:      cfg.WebDAVRoot,
		mux:       http.NewServeMux(),
		startTime: time.Now(),

		negativeCache:   make(map[string]*negativeCacheEntry),
		torrentFailures: make(map[int64]*torrentFailureTracker),
	}
	s.registerRoutes()
	return s
}

// versionHeader returns an HTTP middleware that sets the Server header.
func (s *Server) versionHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "warpbox/"+s.cfg.Version)
		next.ServeHTTP(w, r)
	})
}

// registerRoutes sets up the HTTP handlers for WebDAV methods,
// the HTML browser, branded landing page, and embedded favicon/logo.
func (s *Server) registerRoutes() {
	handler := s.versionHeader(http.HandlerFunc(s.handleWebDAV))
	s.mux.Handle(s.root+"/", handler)
	s.mux.Handle(s.root, handler)

	// Human-browsable HTML directory listing at /http/
	s.mux.Handle("/http/", s.versionHeader(http.HandlerFunc(s.handleHTTP)))
	s.mux.Handle("/http", s.versionHeader(http.HandlerFunc(s.handleHTTP)))

	// Infuse WebDAV endpoint (same content, different URL path).
	s.mux.Handle("/infuse/", s.versionHeader(http.HandlerFunc(s.handleWebDAV)))
	s.mux.Handle("/infuse", s.versionHeader(http.HandlerFunc(s.handleWebDAV)))

	// Log viewer.
	s.mux.Handle("/logs/", s.versionHeader(http.HandlerFunc(s.handleLogs)))
	s.mux.Handle("/logs", s.versionHeader(http.HandlerFunc(s.handleLogs)))

	// Action endpoints (POST-only).
	s.mux.Handle("/actions/", s.versionHeader(http.HandlerFunc(s.handleActions)))

	s.mux.Handle("/", s.versionHeader(http.HandlerFunc(s.handleLanding)))
	s.mux.HandleFunc("/warpbox.png", s.handleLogo)
	s.mux.HandleFunc("/favicon.ico", s.handleLogo)
}

// handleWebDAV dispatches WebDAV methods to the appropriate handler.
// If the request comes via /infuse/, rewrite the path to the configured
// WebDAV root so the sub-handlers work without modification.
func (s *Server) handleWebDAV(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/infuse") {
		r.URL.Path = strings.Replace(r.URL.Path, "/infuse", s.root, 1)
	}

	switch r.Method {
	case http.MethodOptions:
		s.handleOptions(w, r)
	case http.MethodGet:
		s.handleGet(w, r)
	case http.MethodHead:
		s.handleHead(w, r)
	case "PROPFIND":
		s.handlePropfind(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
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

// Start begins listening on the configured address.
func (s *Server) Start(ctx context.Context) error {
	slog.Info("webdav server listening", "addr", s.cfg.ListenAddr)
	return http.ListenAndServe(s.cfg.ListenAddr, s.mux)
}