// Actions — POST endpoints for runtime operations accessible from the
// landing page as buttons (e.g., resync metadata, toggle log level).
package server

import (
	"log/slog"
	"net/http"

	"github.com/ben/warpbox/internal/config"
)

// ActionFunc is a callback that an action button triggers.
type ActionFunc func() error

// actions holds all named action callbacks wired from main.go.
// The map is populated once at startup before any HTTP requests arrive.
var actions = make(map[string]ActionFunc)

// SetActions configures the named action callbacks used by the /actions/ handlers.
func SetActions(funcs map[string]ActionFunc) {
	for name, fn := range funcs {
		actions[name] = fn
	}
}

// handleResync triggers an immediate metadata sync from TorBox.
func (s *Server) handleResync(w http.ResponseWriter, r *http.Request) {
	fn, ok := actions["resync"]
	if !ok {
		http.Error(w, "Resync not configured", http.StatusInternalServerError)
		return
	}

	slog.Info("action: resync triggered from landing page")
	go func() {
		if err := fn(); err != nil {
			slog.Error("action: resync failed", "error", err)
		}
	}()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Resync triggered\n"))
}

// handleRestartSync stops the sync worker loop and starts a fresh one.
func (s *Server) handleRestartSync(w http.ResponseWriter, r *http.Request) {
	fn, ok := actions["restart-sync"]
	if !ok {
		http.Error(w, "Restart sync not configured", http.StatusInternalServerError)
		return
	}

	slog.Info("action: restart-sync triggered from landing page")
	go func() {
		if err := fn(); err != nil {
			slog.Error("action: restart-sync failed", "error", err)
		}
	}()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Sync worker restart triggered\n"))
}

// handleLogLevel changes the runtime log level and persists it to config.yml.
// Accepts form value "level=debug|info|warn|error".
func (s *Server) handleLogLevel(w http.ResponseWriter, r *http.Request) {
	newLevel := r.FormValue("level")
	if newLevel == "" {
		http.Error(w, "Missing 'level' parameter", http.StatusBadRequest)
		return
	}

	// Validate and parse the level.
	parsedLevel, err := config.ParseLevel(newLevel)
	if err != nil {
		http.Error(w, "Invalid level: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Persist to config file.
	cfgPath := s.ConfigPath()
	if cfgPath != "" {
		if err := config.UpdateLogLevel(cfgPath, newLevel); err != nil {
			slog.Error("action: failed to persist log level to config", "path", cfgPath, "error", err)
			http.Error(w, "Failed to persist log level", http.StatusInternalServerError)
			return
		}
	}

	// Atomically swap the log level at runtime via LevelVar.
	// This takes effect immediately for all slog handlers that reference it.
	s.cfg.LevelVar.Set(parsedLevel)

	slog.Info("action: log level changed", "level", newLevel)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Log level changed to " + newLevel + "\n"))
}