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
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"time"
)

//go:embed landing.html warpbox-sm.png
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
	FileCount            int
	WebDAVURL            string
	HTTPURL              string
	InfuseURL            string
	LogsURL              string
	AllocMB              uint64
	TotalAllocMB         uint64
	SysMB                uint64
	NumGC                uint64
	ListenAddr           string
	WebDAVRoot           string
	MaxRAMMB             int
	ChunkSizeMB          int
	TTLSeconds           int
	EvictionStrategy     string
	CDNURLTTLMinutes     int
	RequestsPerMinute    int
	SyncIntervalMinutes  int
	LogFormat            string
	LogLevel             string
	APICallsTotal        int64
	APICallsLastMinute   int
	LastSyncTime         string // human-readable time of last successful sync
	LastSyncError        string // empty if last sync succeeded
	APIBad               bool   // true if there's a sync error to highlight
}

// handleLanding serves the Warpbox branded landing page with runtime stats.
func (s *Server) handleLanding(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// Gather runtime data.
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	uptime := time.Since(s.startTime)
	uptimeStr := formatDuration(uptime)

	// Count files in the store.
	fileCount, err := s.store.CountFiles()
	if err != nil {
		slog.Error("landing: CountFiles failed", "error", err)
		fileCount = -1
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

	data := LandingData{
		Version:             s.cfg.Version,
		Uptime:              uptimeStr,
		FileCount:           fileCount,
		WebDAVURL:           s.root + "/",
		HTTPURL:             "/http/",
		InfuseURL:           "/infuse/",
		LogsURL:             "/logs/",
		AllocMB:             mem.Alloc / 1024 / 1024,
		TotalAllocMB:        mem.TotalAlloc / 1024 / 1024,
		SysMB:               mem.Sys / 1024 / 1024,
		NumGC:               uint64(mem.NumGC),
		ListenAddr:          s.cfg.ListenAddr,
		WebDAVRoot:          s.cfg.WebDAVRoot,
		MaxRAMMB:            s.cfg.MaxRAMMB,
		ChunkSizeMB:         s.cfg.ChunkSizeMB,
		TTLSeconds:          s.cfg.TTLSeconds,
		EvictionStrategy:    s.cfg.EvictionStrategy,
		CDNURLTTLMinutes:    s.cfg.CDNTtlMinutes,
		RequestsPerMinute:   s.cfg.RequestsPerMinute,
		SyncIntervalMinutes: s.cfg.SyncIntervalMinute,
		LogFormat:           s.cfg.LogFormat,
		LogLevel:            s.cfg.LogLevel,
		APICallsTotal:       throttleStats.TotalCalls,
		APICallsLastMinute:  throttleStats.CallsLastMinute,
		LastSyncTime:        lastSyncTime,
		LastSyncError:       lastSyncErr,
		APIBad:              apiBad,
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

// handleLogo serves the embedded warpbox.png at /warpbox.png and also at
// /favicon.ico, giving the landing page a branded browser tab icon.
func (s *Server) handleLogo(w http.ResponseWriter, r *http.Request) {
	png, err := landingFS.ReadFile("warpbox-sm.png")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, "warpbox.png", time.Time{}, &byteReadSeeker{data: png})
}

// byteReadSeeker wraps a []byte so it can be passed to http.ServeContent.
type byteReadSeeker struct {
	data []byte
	off  int
}

func (b *byteReadSeeker) Read(p []byte) (int, error) {
	if b.off >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.off:])
	b.off += n
	return n, nil
}

func (b *byteReadSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		b.off = int(offset)
	case io.SeekCurrent:
		b.off += int(offset)
	case io.SeekEnd:
		b.off = len(b.data) + int(offset)
	}
	return int64(b.off), nil
}