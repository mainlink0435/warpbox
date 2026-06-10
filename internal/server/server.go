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
	"time"

	"github.com/ben/warpbox/internal/cache"
	"github.com/ben/warpbox/internal/metadata"
	"github.com/ben/warpbox/internal/throttle"
	"github.com/ben/warpbox/internal/torbox"
)

// SyncStatusFunc is a callback that returns the current sync status.
type SyncStatusFunc func() metadata.SyncStatus

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
}

// Config holds the server-specific configuration.
type Config struct {
	ListenAddr         string
	WebDAVRoot         string
	CDNTtlMinutes      int    // How long to cache CDN URLs (0 = disable)
	Version            string // Build version, injected at compile time
	MaxRAMMB           int    // For landing page display
	ChunkSizeMB        int    // For landing page display
	TTLSeconds         int    // For landing page display
	EvictionStrategy   string // For landing page display
	RequestsPerMinute  int    // For landing page display
	LogFormat          string // For landing page display
	LogLevel           string // For landing page display
	SyncIntervalMinute int    // For landing page display
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