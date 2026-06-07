// WebDAV GET handler — serves file content via throttle → cache → CDN pipeline.
//
// Handles byte-range requests for partial content delivery (used by rclone
// for metadata scanning and media server streaming). CDN URLs are cached in
// the SQLite store with configurable TTL to minimise TorBox API calls.
package server

import (
	"context"
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
		http.Error(w, "not found", http.StatusNotFound)
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
		type result struct {
			url string
			err error
		}
		resCh := make(chan result, 1)

		s.queue.Enqueue(throttle.Request{
			Label: fmt.Sprintf("GET CDN URL for file %d", file.FileID),
			Execute: func(ctx context.Context) error {
				url, err := s.torBox.GetDownloadURL(ctx, file.TorrentID, file.FileID, false)
				resCh <- result{url, err}
				return err
			},
		})

		res := <-resCh
		if res.err != nil {
			slog.Error("GET: failed to get CDN URL", "torrent_id", file.TorrentID, "file_id", file.FileID, "error", res.err)
			http.Error(w, "Failed to get download URL", http.StatusBadGateway)
			return
		}
		cdnURL = res.url

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
	slog.Debug("GET: cache miss, proxying from CDN", "id", file.ID, "offset", srvRange.Start)

	client := &http.Client{Timeout: 30 * time.Second}
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
		http.Error(w, "CDN proxy error", http.StatusBadGateway)
		return
	}
	defer proxyResp.Body.Close()

	data, err := io.ReadAll(proxyResp.Body)
	if err != nil {
		slog.Error("GET: failed to read CDN response", "error", err)
		http.Error(w, "CDN read error", http.StatusBadGateway)
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