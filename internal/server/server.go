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

	"github.com/ben/warpbox/internal/cache"
	"github.com/ben/warpbox/internal/metadata"
	"github.com/ben/warpbox/internal/throttle"
	"github.com/ben/warpbox/internal/torbox"
)

// Server is the Warpbox WebDAV server.
type Server struct {
	cfg     Config
	store   *metadata.Store
	cache   *cache.Buffer
	torBox  *torbox.Client
	queue   *throttle.Queue
	root    string
	mux     *http.ServeMux
}

// Config holds the server-specific configuration.
type Config struct {
	ListenAddr string
	WebDAVRoot string
}

// New creates a new WebDAV server.
func New(cfg Config, store *metadata.Store, cache *cache.Buffer, torBox *torbox.Client, queue *throttle.Queue) *Server {
	s := &Server{
		cfg:    cfg,
		store:  store,
		cache:  cache,
		torBox: torBox,
		queue:  queue,
		root:   cfg.WebDAVRoot,
		mux:    http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// registerRoutes sets up the HTTP handlers for WebDAV methods.
// The ServeMux dispatches by path, then handleWebDAV dispatches by method.
func (s *Server) registerRoutes() {
	handler := http.HandlerFunc(s.handleWebDAV)
	s.mux.Handle(s.root+"/", handler)
	s.mux.Handle(s.root, handler)
}

// handleWebDAV dispatches WebDAV methods to the appropriate handler.
func (s *Server) handleWebDAV(w http.ResponseWriter, r *http.Request) {
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

// handleOptions responds with WebDAV capabilities.
func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("DAV", "1")
	w.Header().Set("Allow", "OPTIONS, GET, HEAD, PROPFIND")
	w.WriteHeader(http.StatusOK)
}

// handleGet serves file content (with optional byte-range).
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement byte-range aware file serving via throttle → cache → CDN.
	slog.Debug("GET request", "path", r.URL.Path)
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// handleHead is the no-body variant of GET.
func (s *Server) handleHead(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement HEAD.
	slog.Debug("HEAD request", "path", r.URL.Path)
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// Start begins listening on the configured address.
func (s *Server) Start(ctx context.Context) error {
	slog.Info("webdav server listening", "addr", s.cfg.ListenAddr)
	return http.ListenAndServe(s.cfg.ListenAddr, s.mux)
}