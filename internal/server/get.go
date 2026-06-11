// WebDAV GET handler — serves file content via throttle → cache → CDN pipeline.
//
// Handles byte-range requests for partial content delivery (used by rclone
// for metadata scanning and media server streaming). CDN URLs are cached in
// the SQLite store with configurable TTL to minimise TorBox API calls.
package server

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ben/warpbox/internal/throttle"
)

// ---------------------------------------------------------------------------
// GET handler
// ---------------------------------------------------------------------------

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	// Resolve virtual path.
	virtualPath := strings.TrimPrefix(r.URL.Path, s.root)
	virtualPath = strings.TrimPrefix(virtualPath, "/")

	if virtualPath == "" {
		s.serveDirListing(w, r.URL.Path, "1")
		return
	}

	slog.Debug("GET", "path", virtualPath, "range", r.Header.Get("Range"))

	// Look up the file in the SQLite store.
	file, err := s.store.GetFileByPath(virtualPath)
	if err != nil {
		slog.Error("GET: store lookup failed", "path", virtualPath, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if file == nil {
		// Not a file — check if it's a virtual directory with children.
		records, listErr := s.store.ListDir(virtualPath)
		if listErr != nil {
			slog.Error("GET: ListDir failed", "prefix", virtualPath, "error", listErr)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		if len(records) > 0 {
			s.serveDirListing(w, r.URL.Path, "1")
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Get or refresh the CDN URL.
	cdnURL, err := s.store.GetCDNURL(file.ID)
	if err != nil {
		slog.Error("GET: CDN URL lookup failed", "id", file.ID, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if cdnURL == "" {
		// No cached CDN URL — fetch one via the throttle queue.
		cdnURL, err = s.fetchCDNURL(file.TorrentID, file.FileID)
		if err != nil {
			slog.Error("GET: failed to get CDN URL", "torrent_id", file.TorrentID, "file_id", file.FileID, "error", err)
			http.Error(w, "Failed to get download URL", http.StatusBadGateway)
			return
		}

		// Cache the CDN URL if TTL > 0.
		if s.cfg.CDNTtlMinutes > 0 {
			expiry := time.Now().Add(time.Duration(s.cfg.CDNTtlMinutes) * time.Minute)
			if err := s.store.SetCDNURL(file.ID, cdnURL, expiry); err != nil {
				slog.Error("GET: failed to cache CDN URL", "path", file.Path, "error", err)
			}
		}
	}

	// Determine if the client requested a byte range.
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		// No range — redirect directly to the CDN URL.
		slog.Debug("GET: redirecting to CDN", "url", cdnURL)
		http.Redirect(w, r, cdnURL, http.StatusFound)
		return
	}

	// Parse the byte range.
	srvRange, err := parseRange(rangeHeader, file.Size)
	if err != nil {
		slog.Error("GET: invalid range", "range", rangeHeader, "error", err)
		http.Error(w, "Invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// Check the RAM cache first.
	cachedData := s.cache.Get(int(file.ID), srvRange.Start)
	if cachedData != nil {
		slog.Debug("GET: cache hit", "id", file.ID, "offset", srvRange.Start)
		mime := file.MimeType
		if mime == "" {
			mime = "application/octet-stream"
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Content-Length", strconv.FormatInt(srvRange.Length, 10))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", srvRange.Start, srvRange.End, file.Size))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(cachedData)
		return
	}

	// Cache miss — fetch the data through a proxied request to the CDN URL.
	// If the CDN returns 403/404, the URL may be stale. Automatically re-fetch
	// a fresh URL via the throttle queue and retry, up to cdn_url_repair_retries.
	slog.Debug("GET: cache miss, proxying from CDN", "id", file.ID, "offset", srvRange.Start)

	client := &http.Client{Timeout: 30 * time.Second}
	maxAttempts := s.cfg.CDNURLRepairRetries + 1

	for attempt := 0; attempt < maxAttempts; attempt++ {
		proxyReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, cdnURL, http.NoBody)
		if err != nil {
			slog.Error("GET: failed to create CDN request", "error", err)
			http.Error(w, "Failed to create upstream request", http.StatusInternalServerError)
			return
		}
		proxyReq.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", srvRange.Start, srvRange.End))

		proxyResp, err := client.Do(proxyReq)
		if err != nil {
			slog.Error("GET: CDN proxy request failed", "error", err)
			// Network error — do not retry.
			http.Error(w, "CDN proxy error", http.StatusBadGateway)
			return
		}

		// Check for stale CDN URL.
		if (proxyResp.StatusCode == http.StatusForbidden || proxyResp.StatusCode == http.StatusNotFound) &&
			s.cfg.CDNURLAutoRepair && attempt < maxAttempts-1 {

			proxyResp.Body.Close()
			slog.Warn("stale CDN URL detected, refreshing",
				"path", file.Path,
				"attempt", attempt+1,
				"max_retries", s.cfg.CDNURLRepairRetries,
				"status", proxyResp.StatusCode,
			)

			newURL, refreshErr := s.fetchCDNURL(file.TorrentID, file.FileID)
			if refreshErr != nil {
				slog.Error("GET: CDN URL refresh failed",
					"path", file.Path,
					"attempt", attempt+1,
					"error", refreshErr,
				)
				http.Error(w, "CDN URL refresh failed", http.StatusBadGateway)
				return
			}

			// Update the cached CDN URL.
			cdnURL = newURL
			if s.cfg.CDNTtlMinutes > 0 {
				expiry := time.Now().Add(time.Duration(s.cfg.CDNTtlMinutes) * time.Minute)
				if err := s.store.SetCDNURL(file.ID, cdnURL, expiry); err != nil {
					slog.Error("GET: failed to update cached CDN URL", "path", file.Path, "error", err)
				}
			}
			continue // retry with the fresh URL
		}

		// Read the response body on success or non-retryable status.
		data, readErr := io.ReadAll(proxyResp.Body)
		proxyResp.Body.Close()

		if readErr != nil {
			slog.Error("GET: failed to read CDN response", "error", readErr)
			http.Error(w, "CDN read error", http.StatusBadGateway)
			return
		}

		// Non-success status that wasn't repaired — return an error.
		if proxyResp.StatusCode != http.StatusOK && proxyResp.StatusCode != http.StatusPartialContent {
			slog.Error("GET: CDN returned non-success",
				"path", file.Path,
				"status", proxyResp.StatusCode,
			)
			http.Error(w, fmt.Sprintf("CDN returned status %d", proxyResp.StatusCode), http.StatusBadGateway)
			return
		}

		// Cache the chunk in RAM.
		s.cache.Put(int(file.ID), srvRange.Start, data)

		// Serve the partial content.
		mime := file.MimeType
		if mime == "" {
			mime = "application/octet-stream"
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Content-Length", strconv.FormatInt(int64(len(data)), 10))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", srvRange.Start, srvRange.End, file.Size))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data)
		return
	}

	// All attempts exhausted without success.
	http.Error(w, "CDN proxy error after retries", http.StatusBadGateway)
}

// ---------------------------------------------------------------------------
// CDN URL helpers — retry, backoff, negative cache, circuit breaker
// ---------------------------------------------------------------------------

// cdnCacheKey builds a map key from torrent_id and file_id.
func cdnCacheKey(torrentID, fileID int64) string {
	return fmt.Sprintf("%d:%d", torrentID, fileID)
}

// isTorrentStale checks whether a torrent has been marked stale by the circuit
// breaker. Stale torrents skip API calls entirely.
func (s *Server) isTorrentStale(torrentID int64) bool {
	s.torrentFailuresMu.Lock()
	defer s.torrentFailuresMu.Unlock()

	tracker, exists := s.torrentFailures[torrentID]
	if !exists {
		return false
	}
	if time.Now().Before(tracker.staleUntil) {
		slog.Warn("circuit breaker: torrent marked stale, skipping CDN URL fetch",
			"torrent_id", torrentID,
			"stale_until", tracker.staleUntil.Format(time.RFC3339),
		)
		return true
	}
	// Stale period expired — remove the tracker so we try again.
	delete(s.torrentFailures, torrentID)
	slog.Info("circuit breaker: torrent stale period expired, will retry",
		"torrent_id", torrentID,
	)
	return false
}

// recordTorrentFailure records a failure for the given torrent. If the failure
// count exceeds cfg.CircuitBreakerFailures within cfg.CircuitBreakerWindowSec,
// the torrent is marked stale for cfg.CircuitBreakerStaleMin minutes.
func (s *Server) recordTorrentFailure(torrentID int64) {
	s.torrentFailuresMu.Lock()
	defer s.torrentFailuresMu.Unlock()

	now := time.Now()
	tracker, exists := s.torrentFailures[torrentID]
	if !exists {
		tracker = &torrentFailureTracker{}
		s.torrentFailures[torrentID] = tracker
	}

	// Prune failures outside the sliding window.
	window := time.Duration(s.cfg.CircuitBreakerWindowSec) * time.Second
	cutoff := now.Add(-window)
	var active []time.Time
	for _, t := range tracker.failures {
		if t.After(cutoff) {
			active = append(active, t)
		}
	}
	active = append(active, now)
	tracker.failures = active

	if len(active) >= s.cfg.CircuitBreakerFailures {
		staleDur := time.Duration(s.cfg.CircuitBreakerStaleMin) * time.Minute
		tracker.staleUntil = now.Add(staleDur)
		slog.Warn("circuit breaker: torrent exceeded failure threshold, marking stale",
			"torrent_id", torrentID,
			"failures", len(active),
			"window_seconds", window.Seconds(),
			"threshold", s.cfg.CircuitBreakerFailures,
			"stale_duration_minutes", s.cfg.CircuitBreakerStaleMin,
			"stale_until", tracker.staleUntil.Format(time.RFC3339),
		)
	}
}

// getCDNURLWithRetry enqueues a GetDownloadURL call through the throttle queue
// and returns the fresh CDN URL. On failure it retries with exponential backoff
// (cfg.CDNURLRetryBackoff * 1s, * 2s, * 4s, etc.) for up to cfg.CDNURLRetryCount
// attempts. 429 responses use a 5s backoff instead.
func (s *Server) getCDNURLWithRetry(torrentID, fileID int64) (string, error) {
	maxRetries := s.cfg.CDNURLRetryCount
	baseBackoff := time.Duration(s.cfg.CDNURLRetryBackoff) * time.Second

	type result struct {
		url string
		err error
	}

	for attempt := 0; attempt <= maxRetries; attempt++ {
		resCh := make(chan result, 1)

		s.queue.Enqueue(throttle.Request{
			Label: fmt.Sprintf("fetch CDN URL for file %d (attempt %d/%d)", fileID, attempt+1, maxRetries+1),
			Execute: func(ctx context.Context) error {
				url, err := s.torBox.GetDownloadURL(ctx, torrentID, fileID, false)
				resCh <- result{url, err}
				return err
			},
		})

		res := <-resCh

		if res.err == nil {
			return res.url, nil
		}

		// Check if the error is retryable. 429 and 5xx are retryable.
		errStr := res.err.Error()
		isRetryable := strings.Contains(errStr, "unexpected status 429") ||
			strings.Contains(errStr, "unexpected status 5")

		if !isRetryable || attempt >= maxRetries {
			// Non-retryable or out of attempts — record and return.
			s.recordTorrentFailure(torrentID)
			slog.Warn("CDN URL fetch failed, non-retryable or exhausted",
				"torrent_id", torrentID,
				"file_id", fileID,
				"attempt", attempt+1,
				"max_attempts", maxRetries+1,
				"retry_backoff_base", s.cfg.CDNURLRetryBackoff,
				"error", res.err,
			)
			return "", res.err
		}

		// Exponential backoff: base * 2^attempt
		wait := baseBackoff * (1 << attempt)
		// 429 rate-limit errors get a long 30s backoff. Once TorBox rate-limits,
		// we need to give it breathing room rather than retrying aggressively.
		if strings.Contains(errStr, "unexpected status 429") {
			wait = 30 * time.Second
		}
		slog.Warn("CDN URL fetch failed, retrying with backoff",
			"torrent_id", torrentID,
			"file_id", fileID,
			"attempt", attempt+1,
			"max_attempts", maxRetries+1,
			"backoff_seconds", wait.Seconds(),
			"error", res.err,
		)
		time.Sleep(wait)
	}

	return "", fmt.Errorf("torbox: CDN URL fetch exhausted after %d retries", maxRetries)
}

// fetchCDNURL is the public entry point for handleGet to obtain a CDN URL.
// It checks the negative cache and circuit breaker before making any API calls.
func (s *Server) fetchCDNURL(torrentID, fileID int64) (string, error) {
	key := cdnCacheKey(torrentID, fileID)

	// 1. Check negative cache for a recent failure on this exact file.
	s.negativeCacheMu.Lock()
	entry, found := s.negativeCache[key]
	if found {
		if time.Now().Before(entry.expiresAt) {
			s.negativeCacheMu.Unlock()
			slog.Debug("negative cache hit, skipping CDN URL fetch",
				"torrent_id", torrentID,
				"file_id", fileID,
				"error", entry.err,
			)
			return "", entry.err
		}
		// Expired — clean up.
		delete(s.negativeCache, key)
	}
	s.negativeCacheMu.Unlock()

	// 2. Check circuit breaker for this torrent.
	if s.isTorrentStale(torrentID) {
		return "", fmt.Errorf("torrent %d is marked stale by circuit breaker", torrentID)
	}

	// 3. Attempt the API call with retry.
	cdnURL, err := s.getCDNURLWithRetry(torrentID, fileID)
	if err != nil {
		// Cache the error in the negative cache so subsequent requests for the
		// same file fail fast without hitting the API.
		ttl := time.Duration(s.cfg.NegativeCacheTTLSeconds) * time.Second
		s.negativeCacheMu.Lock()
		s.negativeCache[key] = &negativeCacheEntry{
			err:       err,
			expiresAt: time.Now().Add(ttl),
		}
		s.negativeCacheMu.Unlock()
		return "", err
	}

	return cdnURL, nil
}

// ---------------------------------------------------------------------------
// Byte range parsing
// ---------------------------------------------------------------------------

type httpRange struct {
	Start  int64
	End    int64
	Length int64
}

// parseRange parses a "bytes=start-end" Range header and returns the computed
// range bounds. Only a single range is supported (rclone uses single ranges).
func parseRange(rang string, fileSize int64) (*httpRange, error) {
	if rang == "" {
		return nil, fmt.Errorf("empty range")
	}

	if !strings.HasPrefix(rang, "bytes=") {
		return nil, fmt.Errorf("invalid range prefix")
	}

	rangeVal := strings.TrimPrefix(rang, "bytes=")
	parts := strings.SplitN(rangeVal, "-", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid range format")
	}

	startStr := strings.TrimSpace(parts[0])
	endStr := strings.TrimSpace(parts[1])

	var start, end int64

	if startStr == "" {
		// Suffix range: "bytes=-N" means last N bytes.
		suffixSize, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid suffix range: %w", err)
		}
		if suffixSize >= fileSize {
			start = 0
			end = fileSize - 1
		} else {
			start = fileSize - suffixSize
			end = fileSize - 1
		}
	} else {
		var parseErr error
		start, parseErr = strconv.ParseInt(startStr, 10, 64)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid start in range: %w", parseErr)
		}

		if endStr == "" {
			end = fileSize - 1
		} else {
			end, parseErr = strconv.ParseInt(endStr, 10, 64)
			if parseErr != nil {
				return nil, fmt.Errorf("invalid end in range: %w", parseErr)
			}
		}

		if start > end || start < 0 || end >= fileSize {
			return nil, fmt.Errorf("range out of bounds: start=%d end=%d fileSize=%d", start, end, fileSize)
		}
	}

	return &httpRange{
		Start:  start,
		End:    end,
		Length: end - start + 1,
	}, nil
}

// ---------------------------------------------------------------------------
// HEAD handler (same as GET but no body)
// ---------------------------------------------------------------------------

func (s *Server) handleHead(w http.ResponseWriter, r *http.Request) {
	// Resolve virtual path.
	virtualPath := strings.TrimPrefix(r.URL.Path, s.root)
	virtualPath = strings.TrimPrefix(virtualPath, "/")

	if virtualPath == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	slog.Debug("HEAD", "path", virtualPath)

	// Look up the file to get metadata.
	file, err := s.store.GetFileByPath(virtualPath)
	if err != nil {
		slog.Error("HEAD: store lookup failed", "path", virtualPath, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if file == nil {
		// Not a file — head is for files only; return not found.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	mime := file.MimeType
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------------------
// Directory listing (WebDAV-style Multi-Status for GET on directory paths)
// ---------------------------------------------------------------------------

// serveDirListing responds to a GET request on a virtual directory path with
// a WebDAV Multi-Status XML document listing the directory contents.
// This matches the behaviour of zurg and other standards-compliant WebDAV servers
// so that Chrome and other browsers render a browsable directory listing.
func (s *Server) serveDirListing(w http.ResponseWriter, reqPath string, depth string) {
	slog.Debug("directory listing", "path", reqPath, "depth", depth)

	// Normalise the path.
	normalised := strings.TrimRight(reqPath, "/")
	if normalised == "" {
		normalised = "/"
	}

	// Build the virtual prefix: strip the WebDAV root from the path.
	prefix := strings.TrimPrefix(normalised, s.root)
	prefix = strings.TrimPrefix(prefix, "/")

	// List files from SQLite matching this prefix.
	records, err := s.store.ListDir(prefix)
	if err != nil {
		slog.Error("directory listing: ListDir failed", "prefix", prefix, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Build a set of virtual paths for the response.
	seen := map[string]bool{}
	var responses []response

	// Always include the requested directory itself.
	dirHref := normalised
	if !strings.HasSuffix(dirHref, "/") {
		dirHref += "/"
	}
	responses = appendResponse(responses, dirHref, true, 0, "", "", "", &seen)

	// Add immediate children based on depth.
	if depth == "1" || depth == "infinity" {
		// Track immediate children of the requested directory.
		type childInfo struct {
			isDir     bool
			size      int64
			name      string
			mime      string
			createdAt string
		}
		immediate := map[string]childInfo{}

		for _, rec := range records {
			relPath := strings.TrimPrefix(rec.Path, prefix)
			relPath = strings.TrimPrefix(relPath, "/")

			parts := strings.SplitN(relPath, "/", 2)
			immediateName := parts[0]

			if _, exists := immediate[immediateName]; exists {
				continue
			}

			if len(parts) > 1 {
				// The file is nested deeper — the immediate child is a directory.
				immediate[immediateName] = childInfo{isDir: true}
			} else {
				// Direct file in the requested directory.
				mime := rec.MimeType
				if mime == "" {
					mime = "application/octet-stream"
				}
				immediate[immediateName] = childInfo{
					isDir:     false,
					size:      rec.Size,
					name:      rec.Name,
					mime:      mime,
					createdAt: rec.CreatedAt,
				}
			}
		}

		// Build response entries from the immediate children map.
		baseHref := strings.TrimRight(normalised, "/") + "/"
		for name, info := range immediate {
			childHref := baseHref + name
			if info.isDir {
				childHref += "/"
				responses = appendResponse(responses, childHref, true, 0, "", "", "", &seen)
			} else {
				responses = appendResponse(responses, childHref, false, info.size, info.name, info.mime, info.createdAt, &seen)
			}
		}
	}

	// Build the XML response.
	ms := multiStatus{
		XmlnsD:    davNamespace,
		Responses: responses,
	}

	output, err := xml.MarshalIndent(ms, "", "  ")
	if err != nil {
		slog.Error("directory listing: XML marshal failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Prepend XML declaration.
	body := append([]byte(xml.Header), output...)

	w.Header().Set("DAV", "1")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	w.Write(body)
}