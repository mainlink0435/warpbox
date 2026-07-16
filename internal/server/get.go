// WebDAV GET handler — serves file content via throttle → CDN pipeline.
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

	"github.com/mainlink0435/warpbox/internal/library"
	"github.com/mainlink0435/warpbox/internal/metadata"
	"github.com/mainlink0435/warpbox/internal/throttle"
	"github.com/mainlink0435/warpbox/internal/torbox"
)

// ---------------------------------------------------------------------------
// GET handler
// ---------------------------------------------------------------------------

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	// Extract filter from context (set by virtual path handlers).
	libFilter, _ := r.Context().Value(filterKey).(*library.Filter)

	// Resolve virtual path using the appropriate root (mount or s.root).
	root := s.rootForRequest(r)
	virtualPath := strings.TrimPrefix(r.URL.Path, root)
	virtualPath = strings.TrimPrefix(virtualPath, "/")

	if virtualPath == "" {
		s.serveDirListing(w, r.URL.Path, "1", libFilter, root)
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
			s.serveDirListing(w, r.URL.Path, "1", libFilter, root)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// For WebDAV, if there's no Range header, redirect directly to the CDN
	// rather than proxying (preserves the existing behaviour and avoids
	// consuming a CDN connection slot for full-file downloads).
	// The /http/ endpoint (handleHTTP) always proxies, even without Range,
	// because browsers need proper Content-Type headers for inline playback.
	if r.Header.Get("Range") == "" {
		cdnURL, cdnErr := s.store.GetCDNURL(file.ID)
		if cdnErr == nil && cdnURL != "" {
			slog.Debug("GET: redirecting to CDN", "id", file.ID)
			http.Redirect(w, r, cdnURL, http.StatusFound)
			return
		}
		// If we don't have a cached CDN URL, fall through to streamFileContent
		// which will fetch one and proxy.
	}

	s.streamFileContent(w, r, file)
}

// streamFileContent serves file bytes through the CDN proxy pipeline.
// Used by both handleGet (WebDAV) and handleHTTP (direct streaming).
// It handles CDN URL resolution (with retry, hang/poll, negative cache,
// circuit breaker), byte-range requests, and streaming the response to the
// client via a proxy from the CDN.
func (s *Server) streamFileContent(w http.ResponseWriter, r *http.Request, file *metadata.FileRecord) {
	// Get or refresh the CDN URL.
	cdnURL, err := s.store.GetCDNURL(file.ID)
	if err != nil {
		slog.Error("GET: CDN URL lookup failed", "id", file.ID, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if cdnURL == "" {
		// No cached CDN URL — fetch one via the throttle queue.
		cdnURL, err = s.fetchCDNURL(file.Source, file.ItemID, file.FileID)
		if err != nil {
			// Primary fetch failed — try alternatives (same path, different TorBox item).
			cdnURL, err = s.tryCDNFallback(file.Path)
			if err != nil {
				// All alternatives also failed. Instead of returning an error (which
				// rclone counts toward maxErrorCount=10, causing Plex to trash the file),
				// send success headers immediately and hold the connection while polling
				// for the CDN URL. This looks like a slow spinning disk to Plex.
				slog.Warn("GET: CDN URL unavailable (primary + alternatives), entering hang/poll mode",
					"path", file.Path,
					"source", file.Source,
					"item_id", file.ItemID,
					"file_id", file.FileID,
					"alternatives", file.Path != "",
					"error", err,
				)
				s.handleGetCDNHang(w, r, file)
				return
			}
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
	var srvRange *httpRange
	var isRange bool
	if rangeHeader != "" {
		var parseErr error
		srvRange, parseErr = parseRange(rangeHeader, file.Size)
		if parseErr != nil {
			slog.Error("stream: invalid range", "range", rangeHeader, "error", parseErr)
			http.Error(w, "Invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		isRange = true
	} else {
		// No range — serve the full file.
		srvRange = &httpRange{
			Start:  0,
			End:    file.Size - 1,
			Length: file.Size,
		}
	}

	// Fetch the data through a proxied request to the CDN URL.
	// If the CDN returns 403/404, the URL may be stale. Automatically re-fetch
	// a fresh URL via the throttle queue and retry, up to cdn_url_repair_retries.
	slog.Debug("GET: proxying from CDN", "id", file.ID, "offset", srvRange.Start)

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

		// Acquire CDN connection semaphore BEFORE the upstream request so
		// max_cdn_connections actually limits concurrent TorBox CDN opens.
		s.AcquireCDNConn()

		proxyResp, err := client.Do(proxyReq)
		if err != nil {
			s.ReleaseCDNConn()
			slog.Error("GET: CDN proxy request failed", "error", err)
			// Network error — do not retry.
			http.Error(w, "CDN proxy error", http.StatusBadGateway)
			return
		}

		// Check for stale CDN URL.
		if (proxyResp.StatusCode == http.StatusForbidden || proxyResp.StatusCode == http.StatusNotFound) &&
			s.cfg.CDNURLAutoRepair && attempt < maxAttempts-1 {

			s.ReleaseCDNConn()
			proxyResp.Body.Close()
			slog.Warn("stale CDN URL detected, refreshing",
				"path", file.Path,
				"attempt", attempt+1,
				"max_retries", s.cfg.CDNURLRepairRetries,
				"status", proxyResp.StatusCode,
			)

			newURL, refreshErr := s.fetchCDNURL(file.Source, file.ItemID, file.FileID)
			if refreshErr != nil {
				// Primary refresh failed — try alternatives.
				newURL, refreshErr = s.tryCDNFallback(file.Path)
				if refreshErr != nil {
					slog.Error("GET: CDN URL refresh failed (primary + alternatives)",
						"path", file.Path,
						"attempt", attempt+1,
						"error", refreshErr,
					)
					http.Error(w, "CDN URL refresh failed", http.StatusBadGateway)
					return
				}
				slog.Info("CDN URL refresh succeeded via alternative item",
					"path", file.Path,
				)
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

		// 429 (rate limit) and 5xx (server error) from the CDN are transient.
		// Instead of returning 502 (which rclone counts as an error toward
		// maxErrorCount=10), route into hang/poll mode. The cached CDN URL is
		// invalidated so the poll loop will re-fetch a fresh URL and give the
		// CDN time to drain connections.
		if proxyResp.StatusCode == http.StatusTooManyRequests ||
			proxyResp.StatusCode >= 500 {
			s.ReleaseCDNConn()
			proxyResp.Body.Close()
			slog.Warn("GET: CDN transient error, entering hang/poll mode",
				"path", file.Path,
				"status", proxyResp.StatusCode,
				"source", file.Source,
				"item_id", file.ItemID,
				"file_id", file.FileID,
			)
			// Invalidate the cached CDN URL so the hang loop fetches a fresh one.
			if s.cfg.CDNTtlMinutes > 0 {
				expiry := time.Now().Add(-1 * time.Hour)
				if err := s.store.SetCDNURL(file.ID, "", expiry); err != nil {
					slog.Error("GET: failed to invalidate CDN URL cache",
						"path", file.Path, "error", err)
				}
			}
			s.handleGetCDNHang(w, r, file)
			return
		}

		// Check for non-success status that won't be repaired.
		if proxyResp.StatusCode != http.StatusOK && proxyResp.StatusCode != http.StatusPartialContent {
			s.ReleaseCDNConn()
			proxyResp.Body.Close()

			// If the CDN returned 403/404 even after refreshing the URL, the
			// file is not available. Cache this failure so subsequent requests
			// hit the negative cache and skip the API entirely instead of
			// burning TorBox calls.
			if (proxyResp.StatusCode == http.StatusForbidden || proxyResp.StatusCode == http.StatusNotFound) &&
				attempt == maxAttempts-1 {
				key := cdnCacheKey(file.Source, file.ItemID, file.FileID)
				ttl := time.Duration(s.cfg.NegativeCacheTTLSeconds) * time.Second
				s.negativeCacheMu.Lock()
				s.negativeCache[key] = &negativeCacheEntry{
					err:       fmt.Errorf("CDN returned %d after %d repair attempts", proxyResp.StatusCode, maxAttempts),
					expiresAt: time.Now().Add(ttl),
				}
				s.negativeCacheMu.Unlock()
				slog.Warn("CDN proxy exhausted, caching failure in negative cache",
					"path", file.Path,
					"status", proxyResp.StatusCode,
					"attempts", maxAttempts,
					"negative_cache_ttl", ttl.Seconds(),
				)
			}

			slog.Error("GET: CDN returned non-success",
				"path", file.Path,
				"status", proxyResp.StatusCode,
			)
			http.Error(w, fmt.Sprintf("CDN returned status %d", proxyResp.StatusCode), http.StatusBadGateway)
			return
		}

		// TorBox's CDN sometimes returns HTTP 200/206 with a TEXT error body
		// (e.g. "Too many requests" or an HTML page) instead of a proper
		// 429/5xx status when it is rate-limiting or erroring. The status
		// checks above treat that as success, so io.Copy below streams the
		// error text to the client. Under rclone's vfs-cache (cache-mode
		// full) that error text gets written to the cache AS THE FILE'S DATA,
		// permanently corrupting the file until the cache entry is purged:
		// ffprobe then reports duration=N/A / mis-detects the format, Plex
		// playback fails, and *arrs flag good remuxes as "Sample". Real CDN
		// media is binary; a text/html/json content-type is an error body, so
		// treat it like a transient 429 (invalidate the URL, hang/poll).
		if isCDNDisguisedErrorBody(proxyResp.Header.Get("Content-Type")) {
			ct := proxyResp.Header.Get("Content-Type")
			s.ReleaseCDNConn()
			proxyResp.Body.Close()
			slog.Warn("GET: CDN returned a text/error body on a 2xx data response (disguised rate-limit/error) — not streaming, entering hang/poll",
				"path", file.Path, "content_type", ct, "status", proxyResp.StatusCode,
				"source", file.Source, "item_id", file.ItemID, "file_id", file.FileID,
			)
			if s.cfg.CDNTtlMinutes > 0 {
				expiry := time.Now().Add(-1 * time.Hour)
				if err := s.store.SetCDNURL(file.ID, "", expiry); err != nil {
					slog.Error("GET: failed to invalidate CDN URL cache", "path", file.Path, "error", err)
				}
			}
			s.handleGetCDNHang(w, r, file)
			return
		}

		// Stream the CDN response directly to the client.
		mime := file.MimeType
		if mime == "" {
			mime = "application/octet-stream"
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Content-Length", strconv.FormatInt(srvRange.Length, 10))
		w.Header().Set("Accept-Ranges", "bytes")
		if isRange {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", srvRange.Start, srvRange.End, file.Size))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.WriteHeader(http.StatusOK)
		}

		defer s.ReleaseCDNConn()
		defer proxyResp.Body.Close()

		// Stream from CDN → client.
		written, copyErr := io.Copy(w, proxyResp.Body)

		if copyErr != nil {
			// context canceled / broken pipe / connection reset are normal
			// client-side disconnects (Plex seeking, buffering, switching
			// streams). Only show at DEBUG level — they're not actionable.
			slog.Debug("GET: error streaming CDN data",
				"path", file.Path,
				"written", written,
				"error", copyErr,
			)
			return
		}
		return
	}

	// All attempts exhausted without success.
	http.Error(w, "CDN proxy error after retries", http.StatusBadGateway)
}

// ---------------------------------------------------------------------------
// CDN URL helpers — retry, backoff, negative cache, circuit breaker
// ---------------------------------------------------------------------------

// cdnCacheKey builds a map key from source, item_id, and file_id.
func cdnCacheKey(source metadata.FileSource, itemID, fileID int64) string {
	src := "torrent"
	if source == metadata.SourceUsenet {
		src = "usenet"
	}
	return fmt.Sprintf("%s:%d:%d", src, itemID, fileID)
}

// isTorrentStale checks whether a torrent has been marked stale by the circuit
// breaker. Stale torrents skip API calls entirely.
func (s *Server) isTorrentStale(itemID int64) bool {
	s.torrentFailuresMu.Lock()
	defer s.torrentFailuresMu.Unlock()

	tracker, exists := s.torrentFailures[itemID]
	if !exists {
		return false
	}
	if !tracker.staleUntil.IsZero() {
		if time.Now().Before(tracker.staleUntil) {
			slog.Warn("circuit breaker: item marked stale, skipping CDN URL fetch",
				"item_id", itemID,
				"stale_until", tracker.staleUntil.Format(time.RFC3339),
			)
			return true
		}
		// Stale period expired — remove the tracker so we try again.
		delete(s.torrentFailures, itemID)
		slog.Info("circuit breaker: item stale period expired, will retry",
			"item_id", itemID,
		)
	}
	return false
}

// recordTorrentFailure records a failure for the given item (torrent or usenet).
// If the failure count exceeds cfg.CircuitBreakerFailures within
// cfg.CircuitBreakerWindowSec, the item is marked stale for
// cfg.CircuitBreakerStaleMin minutes.
func (s *Server) recordTorrentFailure(itemID int64) {
	s.torrentFailuresMu.Lock()
	defer s.torrentFailuresMu.Unlock()

	now := time.Now()
	tracker, exists := s.torrentFailures[itemID]
	if !exists {
		tracker = &torrentFailureTracker{}
		s.torrentFailures[itemID] = tracker
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
		slog.Warn("circuit breaker: item exceeded failure threshold, marking stale",
			"item_id", itemID,
			"failures", len(active),
			"window_seconds", window.Seconds(),
			"threshold", s.cfg.CircuitBreakerFailures,
			"stale_duration_minutes", s.cfg.CircuitBreakerStaleMin,
			"stale_until", tracker.staleUntil.Format(time.RFC3339),
		)
	}
}

// getCDNURLWithRetry enqueues a TorBox requestdl call through the throttle
// queue and returns the fresh CDN URL. Routes to the torrent or usenet
// endpoint based on source. On failure it retries with exponential backoff
// (cfg.CDNURLRetryBackoff * 1s, * 2s, * 4s, etc.) for up to
// cfg.CDNURLRetryCount attempts. 429 responses use a 5s backoff instead.
func (s *Server) getCDNURLWithRetry(source metadata.FileSource, itemID, fileID int64) (string, error) {
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
				var url string
				var err error
				if source == metadata.SourceUsenet {
					url, err = s.torBox.GetUsenetDownloadURL(ctx, itemID, fileID, false)
				} else {
					url, err = s.torBox.GetDownloadURL(ctx, itemID, fileID, false)
				}
				resCh <- result{url, err}
				return err
			},
		})

		res := <-resCh

		if res.err == nil {
			return res.url, nil
		}

		// Check if the error is retryable. 429, 5xx, timeouts, HTML
		// responses, and network errors can all be transient.
		isRetryable := torbox.IsRetryable(res.err)

		if !isRetryable || attempt >= maxRetries {
			// Non-retryable or out of attempts — record and return.
			s.recordTorrentFailure(itemID)
			slog.Warn("CDN URL fetch failed, non-retryable or exhausted",
				"item_id", itemID,
				"file_id", fileID,
				"source", source,
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
		if strings.Contains(res.err.Error(), "unexpected status 429") {
			wait = 30 * time.Second
		}
		slog.Warn("CDN URL fetch failed, retrying with backoff",
			"item_id", itemID,
			"file_id", fileID,
			"source", source,
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
func (s *Server) fetchCDNURL(source metadata.FileSource, itemID, fileID int64) (string, error) {
	key := cdnCacheKey(source, itemID, fileID)

	// 1. Check negative cache for a recent failure on this exact file.
	s.negativeCacheMu.Lock()
	entry, found := s.negativeCache[key]
	if found {
		if time.Now().Before(entry.expiresAt) {
			s.negativeCacheMu.Unlock()
			slog.Debug("negative cache hit, skipping CDN URL fetch",
				"source", source,
				"item_id", itemID,
				"file_id", fileID,
				"error", entry.err,
			)
			return "", entry.err
		}
		// Expired — clean up.
		delete(s.negativeCache, key)
	}
	s.negativeCacheMu.Unlock()

	// 2. Check circuit breaker for this item.
	if s.isTorrentStale(itemID) {
		return "", fmt.Errorf("item %d is marked stale by circuit breaker", itemID)
	}

	// 3. Attempt the API call with retry.
	cdnURL, err := s.getCDNURLWithRetry(source, itemID, fileID)
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

// tryCDNFallback queries alternative TorBox items sharing the same virtual
// path and tries to fetch a CDN URL from each in turn. Returns the first
// successful URL. If none succeed, returns the last error.
// This provides resilience when the primary item has been deleted from
// TorBox but alternative duplicates still exist in the database.
func (s *Server) tryCDNFallback(path string) (string, error) {
	alternatives, err := s.store.GetFileAlternatives(path)
	if err != nil {
		return "", fmt.Errorf("querying alternatives: %w", err)
	}
	if len(alternatives) == 0 {
		return "", fmt.Errorf("no alternatives for path %q", path)
	}

	var lastErr error
	for _, alt := range alternatives {
		altURL, altErr := s.fetchCDNURL(alt.Source, alt.ItemID, alt.FileID)
		if altErr == nil {
			slog.Info("CDN URL obtained from alternative item",
				"path", path,
				"alt_source", alt.Source,
				"alt_item_id", alt.ItemID,
				"alt_file_id", alt.FileID,
			)
			return altURL, nil
		}
		lastErr = altErr
	}
	return "", fmt.Errorf("all alternatives failed: %w", lastErr)
}

// isCDNDisguisedErrorBody reports whether a CDN Content-Type looks like an
// error page rather than binary media (TorBox sometimes returns 200 with
// text rate-limit bodies).
func isCDNDisguisedErrorBody(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.HasPrefix(ct, "text/") || strings.Contains(ct, "html") || strings.Contains(ct, "json")
}

// cdnPollInterval is how long to wait between CDN URL fetch attempts when
// the CDN is unavailable and we are hanging the connection open.
const cdnPollInterval = 15 * time.Second

// handleGetCDNHang is entered when the CDN URL cannot be fetched and we want
// to avoid returning an error (which rclone counts toward maxErrorCount=10).
//
// It sends success HTTP headers immediately, then polls fetchCDNURL with
// exponential backoff when rate-limited (starts at cdnPollInterval, doubles
// on each 429 to a 5-minute max). Once a URL is obtained, it proxies the
// file data from the CDN transparently.
//
// If the client disconnects (context cancelled), we clean up and exit.
// If rclone's --timeout (default 5m) fires, the connection drops and rclone
// counts one error, but at 1 per 5 minutes it would take 50+ to hit
// maxErrorCount=10.
func (s *Server) handleGetCDNHang(w http.ResponseWriter, r *http.Request, file *metadata.FileRecord) {
	// Parse the byte range, if present.
	rangeHeader := r.Header.Get("Range")
	var srvRange *httpRange
	var hasRange bool
	if rangeHeader != "" {
		var parseErr error
		srvRange, parseErr = parseRange(rangeHeader, file.Size)
		if parseErr != nil {
			slog.Error("GET (hang): invalid range", "range", rangeHeader, "path", file.Path, "error", parseErr)
			http.Error(w, "Invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		hasRange = true
	} else {
		// Synthesize a full-file range.
		srvRange = &httpRange{
			Start:  0,
			End:    file.Size - 1,
			Length: file.Size,
		}
	}

	mime := file.MimeType
	if mime == "" {
		mime = "application/octet-stream"
	}

	// Send success headers immediately so rclone sees a successful connection.
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.FormatInt(srvRange.Length, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	if hasRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", srvRange.Start, srvRange.End, file.Size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	// Poll for CDN URL and proxy data, retrying on both requestdl and data errors.
	pollInterval := cdnPollInterval
	const maxPollInterval = 5 * time.Minute
	proxyClient := &http.Client{Timeout: 30 * time.Second}

	for {
		// 1. Fetch a CDN URL (with exponential backoff on requestdl 429).
		cdnURL, fetchErr := s.fetchCDNURL(file.Source, file.ItemID, file.FileID)
		if fetchErr != nil {
			cdnURL, fetchErr = s.tryCDNFallback(file.Path)
		}
		if fetchErr != nil {
			if strings.Contains(fetchErr.Error(), "unexpected status 429") {
				pollInterval *= 2
				if pollInterval > maxPollInterval {
					pollInterval = maxPollInterval
				}
				slog.Warn("GET (hang): rate-limited, increasing poll backoff",
					"path", file.Path,
					"source", file.Source,
					"item_id", file.ItemID,
					"file_id", file.FileID,
					"next_poll", pollInterval,
					"error", fetchErr,
				)
			} else {
				slog.Debug("GET (hang): CDN URL still unavailable",
					"path", file.Path, "error", fetchErr, "next_poll", pollInterval,
				)
			}
			select {
			case <-r.Context().Done():
				slog.Debug("client disconnected while waiting for CDN", "path", file.Path)
				return
			case <-time.After(pollInterval):
			}
			continue
		}

		// 2. Cache the recovered CDN URL.
		if s.cfg.CDNTtlMinutes > 0 {
			expiry := time.Now().Add(time.Duration(s.cfg.CDNTtlMinutes) * time.Minute)
			if err := s.store.SetCDNURL(file.ID, cdnURL, expiry); err != nil {
				slog.Error("GET (hang): failed to cache CDN URL after recovery", "path", file.Path, "error", err)
			}
		}

		slog.Info("CDN URL recovered, attempting data proxy",
			"path", file.Path,
			"source", file.Source,
			"item_id", file.ItemID,
			"file_id", file.FileID,
		)

		// 3. Proxy data from CDN — check for transient errors and retry.
		s.AcquireCDNConn()
		proxyReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, cdnURL, http.NoBody)
		if err != nil {
			s.ReleaseCDNConn()
			slog.Error("GET (hang): failed to create CDN proxy request", "path", file.Path, "error", err)
			return
		}
		proxyReq.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", srvRange.Start, srvRange.End))

		proxyResp, err := proxyClient.Do(proxyReq)
		if err != nil {
			s.ReleaseCDNConn()
			slog.Warn("GET (hang): CDN proxy request failed, will retry",
				"path", file.Path, "error", err, "next_poll", pollInterval,
			)
			select {
			case <-r.Context().Done():
				return
			case <-time.After(pollInterval):
			}
			continue
		}

		// Transient data error — do not stream error body; back off and retry.
		if proxyResp.StatusCode == http.StatusTooManyRequests ||
			proxyResp.StatusCode >= 500 ||
			isCDNDisguisedErrorBody(proxyResp.Header.Get("Content-Type")) {
			status := proxyResp.StatusCode
			ct := proxyResp.Header.Get("Content-Type")
			proxyResp.Body.Close()
			s.ReleaseCDNConn()

			// Invalidate the cached URL so the next loop iteration fetches fresh.
			if s.cfg.CDNTtlMinutes > 0 {
				expiry := time.Now().Add(-1 * time.Hour)
				if err := s.store.SetCDNURL(file.ID, "", expiry); err != nil {
					slog.Error("GET (hang): failed to invalidate CDN URL cache",
						"path", file.Path, "error", err)
				}
			}

			pollInterval *= 2
			if pollInterval > maxPollInterval {
				pollInterval = maxPollInterval
			}
			slog.Warn("GET (hang): CDN data transient error, backing off",
				"path", file.Path,
				"status", status,
				"content_type", ct,
				"source", file.Source,
				"item_id", file.ItemID,
				"file_id", file.FileID,
				"next_poll", pollInterval,
			)
			select {
			case <-r.Context().Done():
				return
			case <-time.After(pollInterval):
			}
			continue
		}

		// Non-recoverable response — log and exit (headers already sent).
		if proxyResp.StatusCode != http.StatusOK && proxyResp.StatusCode != http.StatusPartialContent {
			proxyResp.Body.Close()
			s.ReleaseCDNConn()
			slog.Error("GET (hang): CDN returned non-success after recovery",
				"path", file.Path, "status", proxyResp.StatusCode,
			)
			return
		}

		// 4. Success — stream data to client and exit.
		defer s.ReleaseCDNConn()
		defer proxyResp.Body.Close()
		written, copyErr := io.Copy(w, proxyResp.Body)
		if copyErr != nil {
			slog.Debug("GET (hang): error streaming CDN data", "path", file.Path, "written", written, "error", copyErr)
		} else {
			slog.Debug("GET (hang): finished streaming", "path", file.Path, "bytes", written)
		}
		return
	}
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
	// Resolve virtual path using the appropriate root (mount or s.root).
	root := s.rootForRequest(r)
	virtualPath := strings.TrimPrefix(r.URL.Path, root)
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
func (s *Server) serveDirListing(w http.ResponseWriter, reqPath string, depth string, f *library.Filter, root string) {
	slog.Debug("directory listing", "path", reqPath, "depth", depth)

	// Normalise the path.
	normalised := strings.TrimRight(reqPath, "/")
	if normalised == "" {
		normalised = "/"
	}

	// Build the virtual prefix: strip the root (s.root or mount) from the path.
	prefix := strings.TrimPrefix(normalised, root)
	prefix = strings.TrimPrefix(prefix, "/")

	// List files from SQLite matching this prefix.
	records, err := s.store.ListDir(prefix)
	if err != nil {
		slog.Error("directory listing: ListDir failed", "prefix", prefix, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Apply library filter if provided.
	if f != nil {
		records = f.Apply(records)
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

		// At the root level (/webdav/) with virtual paths configured,
		// show synthetic directory entries instead of real files.
		if prefix == "" && root == webdavRoot {
			baseHref := strings.TrimRight(normalised, "/") + "/"
			responses = appendResponse(responses, baseHref+"__all__/", true, 0, "", "", "", &seen)
			for _, vf := range s.virtualFilters {
				name := strings.TrimPrefix(vf.Mount, "/")
				responses = appendResponse(responses, baseHref+name+"/", true, 0, "", "", "", &seen)
			}
		} else {
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
		} // close else block
	} // close depth block

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
