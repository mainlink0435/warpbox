// Package server implements the WebDAV HTTP handler for Warpbox.
//
// This file contains the branded HTML landing page served at the root
// URL (/). The Warpbox logo is compiled into the binary via Go's embed
// package so there are no external file dependencies at runtime.

package server

import (
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"runtime"
	"time"
)

//go:embed landing.html warpbox.svg chart.umd.min.js
var landingFS embed.FS

// landingTmpl is the parsed landing page template.
var landingTmpl = template.Must(template.New("landing").Parse(
	mustReadString(landingFS, "landing.html"),
))

// mustReadString reads an embedded file and returns its contents as a string.
// Panics on failure (called during init via template.Must).
func mustReadString(fs embed.FS, name string) string {
	b, err := fs.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("embedded file %s: %v", name, err))
	}
	return string(b)
}

// LandingData holds the dynamic values rendered into the landing page template.
type LandingData struct {
	Version              string
	Uptime               string
	FileCount            int // Total rows (may include duplicates from multiple TorBox items)
	FileCountUnique      int // Unique virtual paths (what users see in WebDAV)
	ItemCount            int
	WebDAVURL            string
	HTTPURL              string
	InfuseURL            string
	LogsURL              string
	AllocMB              uint64
	SysMB                uint64
	NumGC                uint64
	HeapObjects          uint64
	ListenAddr           string
	WebDAVRoot           string
	CDNURLTTLMinutes     int
	RequestsPerMinute    int
	SyncIntervalMinutes  int
	LogFormat            string
	LogLevel             string
	APICallsTotal        int64
	APISuccessfulCalls   int64
	APIFailedCalls       int64
	APICallsLastMinute   int
	LastSyncTime         string // human-readable time of last successful sync
	LastSyncError        string // empty if last sync succeeded
	APIBad               bool   // true if there's a sync error to highlight
	NegativeCacheSize    int    // Current entries in the negative cache
	CircuitBreakerSize   int    // Current entries in the circuit breaker
	DBLockErrors         int64

	// Cache config fields
	CDNURLAutoRepair     string // "on" or "off"
	CDNURLRepairRetries  int
	CDNURLRetryBackoff   int    // in seconds
	CDNURLRetryCount     int
	NegativeCacheTTL     int    // in seconds
	CircuitBreakerFails  int
	CircuitBreakerWindow int    // in seconds
	CircuitBreakerStale  int    // in minutes
	NegCacheMaxEntries   int
	CbMaxEntries         int
	CleanupInterval      int    // in seconds
	MaxCDNConnections    int

	// Sync config fields
	SyncListPageSize int

	// Auth config fields
	AuthEnabled bool // true if HTTP Basic Auth is enabled

	// Stats config fields
	StatsInterval    int // in seconds
	StatsRetention   int // in hours
	StatsChartWindow int // in minutes

	// CSRF token for action forms
	CSRFToken string

	// TorBox account fields
	TBUserID           int64
	TBEmail            string
	TBPlan             int
	TBPlanName         string
	TBIsPremium        bool
	TBPremiumExpires   string
	TBCreatedAt        string
	TBReferralCode     string
	TBPremiumDLimit    int64
	TBTotalDownloaded  int64
	TBTotalEgressed    int64
	TBOverallRatio     float64
	TBHasAccount       bool // whether user info was successfully fetched
}

// handleLanding serves the Warpbox branded landing page with runtime stats.
func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	// Gather runtime data.
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	uptime := time.Since(s.startTime)
	uptimeStr := formatDuration(uptime)

	// Count files and items in the store.
	fileCount, err := s.store.CountFiles()
	if err != nil {
		slog.Error("landing: CountFiles failed", "error", err)
		fileCount = -1
	}
	fileCountUnique, err := s.store.CountDistinctPaths()
	if err != nil {
		slog.Error("landing: CountDistinctPaths failed", "error", err)
		fileCountUnique = -1
	}
	itemCount, err := s.store.CountItems()
	if err != nil {
		slog.Error("landing: CountItems failed", "error", err)
		itemCount = -1
	}

	// Throttle stats.
	throttleStats := s.queue.Stats()

	// Sync status (API health).
	lastSyncTime := ""
	lastSyncErr := ""
	apiBad := false
	if s.syncStatus != nil {
		st := s.syncStatus()
		lastSyncErr = st.LastError
		if lastSyncErr != "" {
			apiBad = true
		}
		if !st.LastSuccess.IsZero() {
			lastSyncTime = formatDuration(time.Since(st.LastSuccess)) + " ago"
		} else {
			lastSyncTime = "never"
		}
	}

	negCacheSize := s.NegativeCacheSize()
	cbSize := s.CircuitBreakerSize()

	autoRepair := "off"
	if s.cfg.CDNURLAutoRepair {
		autoRepair = "on"
	}

	data := LandingData{
		Version:             s.cfg.Version,
		Uptime:              uptimeStr,
		FileCount:           fileCount,
		FileCountUnique:     fileCountUnique,
		ItemCount:           itemCount,
		WebDAVURL:           s.root + "/",
		HTTPURL:             "/http/",
		InfuseURL:           "/infuse/",
		LogsURL:             "/logs/",
		AllocMB:              mem.Alloc / 1024 / 1024,
		SysMB:                mem.Sys / 1024 / 1024,
		NumGC:                uint64(mem.NumGC),
		HeapObjects:          mem.HeapObjects,
		ListenAddr:           s.cfg.ListenAddr,
		WebDAVRoot:           webdavRoot,
		CDNURLTTLMinutes:     s.cfg.CDNTtlMinutes,
		RequestsPerMinute:    s.cfg.RequestsPerMinute,
		SyncIntervalMinutes:  s.cfg.SyncIntervalMinute,
		LogFormat:            s.cfg.LogFormat,
		LogLevel:             s.cfg.LogLevel,
		APICallsTotal:        throttleStats.TotalCalls,
		APISuccessfulCalls:   throttleStats.SuccessfulCalls,
		APIFailedCalls:       throttleStats.FailedCalls,
		APICallsLastMinute:   throttleStats.CallsLastMinute,
		LastSyncTime:         lastSyncTime,
		LastSyncError:        lastSyncErr,
		APIBad:               apiBad,
		NegativeCacheSize:    negCacheSize,
		CircuitBreakerSize:   cbSize,
		DBLockErrors:         s.store.DBLockErrors(),

		// Cache config
		CDNURLAutoRepair:     autoRepair,
		CDNURLRepairRetries:  s.cfg.CDNURLRepairRetries,
		CDNURLRetryBackoff:   s.cfg.CDNURLRetryBackoff,
		CDNURLRetryCount:     s.cfg.CDNURLRetryCount,
		NegativeCacheTTL:     s.cfg.NegativeCacheTTLSeconds,
		CircuitBreakerFails:  s.cfg.CircuitBreakerFailures,
		CircuitBreakerWindow: s.cfg.CircuitBreakerWindowSec,
		CircuitBreakerStale:  s.cfg.CircuitBreakerStaleMin,
		NegCacheMaxEntries:   s.cfg.NegativeCacheMaxEntries,
		CbMaxEntries:         s.cfg.CircuitBreakerMaxEntries,
		CleanupInterval:      s.cfg.CleanupIntervalSeconds,
		MaxCDNConnections:    s.cfg.MaxCDNConnections,

		// Auth config
		AuthEnabled: s.cfg.AuthEnabled,
		CSRFToken:   s.csrfToken,

		// Sync config
		SyncListPageSize: s.cfg.SyncListPageSize,

		// Stats config
		StatsInterval:    s.cfg.StatsIntervalSeconds,
		StatsRetention:   s.cfg.StatsRetentionHours,
		StatsChartWindow: s.cfg.StatsChartMinutes,

		// TorBox account
		TBHasAccount: false,
	}

	if ui := s.TorBoxUserInfo(); ui != nil {
		premiumExpires := ""
		if ui.PremiumExpires != nil {
			premiumExpires = *ui.PremiumExpires
		}
		data.TBHasAccount = true
		data.TBUserID = ui.ID
		data.TBEmail = ui.Email
		data.TBPlan = ui.Plan
		data.TBPlanName = ui.PlanName
		data.TBIsPremium = ui.Premium
		data.TBPremiumExpires = premiumExpires
		data.TBCreatedAt = ui.CreatedAt
		data.TBReferralCode = ui.ReferralCode
		data.TBPremiumDLimit = ui.PremiumDownloadLimit
		data.TBTotalDownloaded = ui.TotalDownloaded
		data.TBTotalEgressed = ui.TotalEgressed
		data.TBOverallRatio = ui.OverallRatio
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := landingTmpl.Execute(w, data); err != nil {
		slog.Error("landing: template execute failed", "error", err)
	}
}

// formatDuration returns a human-readable duration string like "2h34m12s".
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// handleChartJS serves the embedded Chart.js bundle at /chart.umd.min.js.
func (s *Server) handleChartJS(w http.ResponseWriter, r *http.Request) {
	b, err := landingFS.ReadFile("chart.umd.min.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(b); err != nil {
		slog.Debug("chart.js write failed", "error", err)
	}
}

// handleLogo serves the embedded warpbox.svg at /warpbox.svg and also at
// /favicon.ico, giving the landing page a branded browser tab icon.
func (s *Server) handleLogo(w http.ResponseWriter, r *http.Request) {
	svg, err := landingFS.ReadFile("warpbox.svg")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(svg); err != nil {
		slog.Debug("logo write failed", "error", err)
	}
}