// Package torbox provides a client for the TorBox API.
//
// Implementation based on the official OpenAPI spec at
// https://api.torbox.app/openapi.json
package torbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// User info types
// ---------------------------------------------------------------------------

// UserInfo represents the TorBox account details from GET /api/user/me.
type UserInfo struct {
	ID              int64   `json:"id"`
	AuthID          string  `json:"auth_id"`
	Email           string  `json:"email"`
	Plan            int     `json:"plan"`
	PlanName        string  `json:"plan_name"`
	Premium         bool    `json:"premium"`
	PremiumExpires  *string `json:"premium_expires,omitempty"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	ReferralCode    string  `json:"referral_code"`
	Registered     bool    `json:"registered"`
	PremiumDownloadLimit int64 `json:"premium_download_limit"`
	TotalDownloaded int64  `json:"total_downloaded"`
	TotalEgressed   int64  `json:"total_egressed"`
	OverallRatio    float64 `json:"overall_ratio"`
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// Client communicates with the TorBox API.
type Client struct {
	baseURL       string
	apiKey        string
	httpClient    *http.Client
	HTTP429Callback func() // Called when a 429 response is received
}

// SetBaseURL overrides the API base URL. Used by tests to redirect traffic
// to an httptest server. Exported but not intended for production use.
func (c *Client) SetBaseURL(url string) { c.baseURL = url }

// SetHTTPClient overrides the HTTP client. Used by tests to inject a
// custom transport or timeout. Exported but not intended for production use.
func (c *Client) SetHTTPClient(hc *http.Client) { c.httpClient = hc }

// NewClient creates a new TorBox API client.
// The base URL is hardcoded to https://api.torbox.app — it is not configurable
// because the TorBox API endpoint is stable and not user-serviceable.
func NewClient(apiKey string) *Client {
	return &Client{
		baseURL: "https://api.torbox.app",
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ---------------------------------------------------------------------------
// API response envelope
// ---------------------------------------------------------------------------

// apiResponse is the standard TorBox API response wrapper.
type apiResponse[T any] struct {
	Data    T       `json:"data"`
	Success *bool   `json:"success,omitempty"`
	Detail  *string `json:"detail,omitempty"`
	Error   *string `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Torrent list types
// ---------------------------------------------------------------------------

// TorrentFile represents a single file within a torrent.
type TorrentFile struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	Size      int64   `json:"size"`
	MimeType  string  `json:"mimetype"`
	S3Path    string  `json:"s3_path"`
	ShortName string  `json:"short_name"`
	MD5       *string `json:"md5,omitempty"`
}

// Torrent represents a torrent from the user's list.
type Torrent struct {
	ID               int64         `json:"id"`
	AuthID           string        `json:"auth_id"`
	Name             string        `json:"name"`
	Hash             string        `json:"hash"`
	Size             int64         `json:"size"`
	DownloadState    string        `json:"download_state"`
	DownloadPresent  bool          `json:"download_present"`
	DownloadSpeed    float64       `json:"download_speed"`
	UploadSpeed      float64       `json:"upload_speed"`
	Progress         float64       `json:"progress"`
	Ratio            float64       `json:"ratio"`
	ETA              float64       `json:"eta"`
	Active           bool          `json:"active"`
	Seeds            float64       `json:"seeds"`
	Peers            float64       `json:"peers"`
	Availability     float64       `json:"availability"`
	Files            []TorrentFile `json:"files"`
	CreatedAt        string        `json:"created_at"`
	UpdatedAt        string        `json:"updated_at"`
	ExpiresAt        string        `json:"expires_at"`
	DownloadFinished bool          `json:"download_finished"`
	TorrentFile      bool          `json:"torrent_file"`
	Server           float64       `json:"server"`
	InactiveCheck    float64       `json:"inactive_check"`
	Magnet           *string       `json:"magnet,omitempty"`
}

// ---------------------------------------------------------------------------
// ListFiles
// ---------------------------------------------------------------------------

// ListFilesParams are optional query parameters for list endpoints.
type ListFilesParams struct {
	BypassCache bool
	Offset      int
	Limit       int
	PageSize    int // Per-request page window; 0 uses defaultListPageSize
}

// defaultListPageSize is the fallback per-request window when paginating mylist
// endpoints. It MUST stay at or below TorBox's per-response cap (observed
// 10,000) so that a full page reliably equals the page size and signals "more
// pages follow" — a page shorter than this marks the end.
const defaultListPageSize = 1000

func (c *Client) listGeneric(ctx context.Context, endpoint, label string, params ListFilesParams) ([]Torrent, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("torbox: invalid base URL: %w", err)
	}

	pageSize := params.PageSize
	if pageSize <= 0 {
		pageSize = defaultListPageSize
	}

	// TorBox's mylist endpoints cap EACH response at ~10,000 items regardless
	// of the requested `limit`. A single un-paginated call therefore silently
	// drops everything past the newest 10k — i.e. the oldest torrents become
	// invisible (accounts with >10k torrents lose their tail, breaking
	// playback of older library content). Page through with offset until a
	// short page signals the end. params.Limit, when > 0, is treated as a
	// safety ceiling on the TOTAL number of items fetched.
	maxTotal := params.Limit
	offset := params.Offset
	var all []Torrent

	for {
		u := base.JoinPath(endpoint)
		q := u.Query()
		q.Set("bypass_cache", strconv.FormatBool(params.BypassCache))
		q.Set("offset", strconv.Itoa(offset))
		q.Set("limit", strconv.Itoa(pageSize))
		u.RawQuery = q.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
		if err != nil {
			return nil, fmt.Errorf("torbox: creating %s request: %w", label, err)
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)

		slog.Debug("torbox "+label, "offset", offset, "limit", pageSize)

		body, err := c.do(req)
		if err != nil {
			return nil, err
		}

		var env apiResponse[[]Torrent]
		if err := json.Unmarshal(body, &env); err != nil {
			if len(body) > 0 && body[0] == '<' {
				snippet := body
				if len(snippet) > 200 {
					snippet = snippet[:200]
				}
				slog.Warn("torbox "+label+": expected JSON, got non-JSON response",
					"endpoint", endpoint,
					"status", 200,
					"body_preview", string(snippet),
				)
				slog.Debug("torbox "+label+": non-JSON response body",
					"endpoint", endpoint,
					"body", truncateBody(body),
				)
			}
			return nil, fmt.Errorf("torbox: decoding %s response: %w", label, err)
		}

		if env.Error != nil && *env.Error != "" {
			return nil, fmt.Errorf("torbox %s API error: %s", label, *env.Error)
		}

		page := env.Data
		all = append(all, page...)
		slog.Debug("torbox "+label+" page", "offset", offset, "got", len(page), "total", len(all))

		if len(page) < pageSize {
			break // short page → end of list
		}
		offset += len(page)
		if maxTotal > 0 && len(all) >= maxTotal {
			all = all[:maxTotal]
			break
		}
	}

	slog.Debug("torbox "+label+" result", "count", len(all))
	return all, nil
}

// ListTorrents returns all torrents and their files from the user's TorBox account.
func (c *Client) ListTorrents(ctx context.Context, params ListFilesParams) ([]Torrent, error) {
	return c.listGeneric(ctx, "/v1/api/torrents/mylist", "torrents", params)
}

// ListUsenet returns all Usenet downloads from the user's TorBox account.
// The API returns the same JSON structure as torrents, so we reuse Torrent.
func (c *Client) ListUsenet(ctx context.Context, params ListFilesParams) ([]Torrent, error) {
	return c.listGeneric(ctx, "/v1/api/usenet/mylist", "usenet", params)
}

// ---------------------------------------------------------------------------
// GetDownloadURL
// ---------------------------------------------------------------------------

// GetDownloadURL returns a CDN download URL for the given file in a torrent.
// The returned URL expires after a few hours but is renewable.
func (c *Client) GetDownloadURL(ctx context.Context, torrentID, fileID int64, redirect bool) (string, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("torbox: invalid base URL: %w", err)
	}
	u := base.JoinPath("/v1/api/torrents/requestdl")
	q := u.Query()
	q.Set("token", c.apiKey)
	q.Set("torrent_id", strconv.FormatInt(torrentID, 10))
	q.Set("file_id", strconv.FormatInt(fileID, 10))
	q.Set("redirect", strconv.FormatBool(redirect))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return "", fmt.Errorf("torbox: creating request: %w", err)
	}

	slog.Debug("torbox get_download_url", "torrent_id", torrentID, "file_id", fileID)

	body, err := c.do(req)
	if err != nil {
		return "", err
	}

	var env apiResponse[string]
	if err := json.Unmarshal(body, &env); err != nil {
		if len(body) > 0 && body[0] == '<' {
			snippet := body
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			slog.Warn("torbox download_url: expected JSON, got non-JSON response",
				"torrent_id", torrentID,
				"file_id", fileID,
				"status", 200,
				"body_preview", string(snippet),
			)
			slog.Debug("torbox download_url: non-JSON response body",
				"torrent_id", torrentID,
				"file_id", fileID,
				"body", truncateBody(body),
			)
		}
		return "", fmt.Errorf("torbox: decoding response: %w", err)
	}

	if env.Error != nil && *env.Error != "" {
		return "", fmt.Errorf("torbox API error: %s", *env.Error)
	}

	slog.Debug("torbox get_download_url result", "has_url", env.Data != "")
	return env.Data, nil
}

// GetUsenetDownloadURL returns a CDN download URL for the given file in a
// Usenet download. Uses /v1/api/usenet/requestdl instead of the torrent endpoint.
func (c *Client) GetUsenetDownloadURL(ctx context.Context, usenetID, fileID int64, redirect bool) (string, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("torbox: invalid base URL: %w", err)
	}
	u := base.JoinPath("/v1/api/usenet/requestdl")
	q := u.Query()
	q.Set("token", c.apiKey)
	q.Set("usenet_id", strconv.FormatInt(usenetID, 10))
	q.Set("file_id", strconv.FormatInt(fileID, 10))
	q.Set("redirect", strconv.FormatBool(redirect))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return "", fmt.Errorf("torbox: creating usenet request: %w", err)
	}

	slog.Debug("torbox get_usenet_download_url", "usenet_id", usenetID, "file_id", fileID)

	body, err := c.do(req)
	if err != nil {
		return "", err
	}

	var env apiResponse[string]
	if err := json.Unmarshal(body, &env); err != nil {
		if len(body) > 0 && body[0] == '<' {
			snippet := body
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			slog.Warn("torbox usenet_download_url: expected JSON, got non-JSON response",
				"usenet_id", usenetID,
				"file_id", fileID,
				"status", 200,
				"body_preview", string(snippet),
			)
			slog.Debug("torbox usenet_download_url: non-JSON response body",
				"usenet_id", usenetID,
				"file_id", fileID,
				"body", truncateBody(body),
			)
		}
		return "", fmt.Errorf("torbox: decoding usenet response: %w", err)
	}

	if env.Error != nil && *env.Error != "" {
		return "", fmt.Errorf("torbox usenet API error: %s", *env.Error)
	}

	slog.Debug("torbox get_usenet_download_url result", "has_url", env.Data != "")
	return env.Data, nil
}

// ---------------------------------------------------------------------------
// GetUserInfo
// ---------------------------------------------------------------------------

// GetUserInfo returns the authenticated user's account details from TorBox.
func (c *Client) GetUserInfo(ctx context.Context) (*UserInfo, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("torbox: invalid base URL: %w", err)
	}
	u := base.JoinPath("/v1/api/user/me")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("torbox: creating user/me request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	slog.Debug("torbox user/me")

	body, err := c.do(req)
	if err != nil {
		return nil, err
	}

	var env apiResponse[UserInfo]
	if err := json.Unmarshal(body, &env); err != nil {
		if len(body) > 0 && body[0] == '<' {
			snippet := body
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			slog.Warn("torbox user/me: expected JSON, got non-JSON response",
				"status", 200,
				"body_preview", string(snippet),
			)
			slog.Debug("torbox user/me: non-JSON response body",
				"body", truncateBody(body),
			)
		}
		return nil, fmt.Errorf("torbox: decoding user/me response: %w", err)
	}

	if env.Error != nil && *env.Error != "" {
		return nil, fmt.Errorf("torbox user/me API error: %s", *env.Error)
	}

	slog.Debug("torbox user/me result", "plan", env.Data.PlanName, "email", env.Data.Email)
	return &env.Data, nil
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

// do executes an HTTP request, reads the full body, and returns the body bytes.
// The response body is always closed before returning.
func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return nil, fmt.Errorf("torbox: request %s %s failed: %w", req.Method, req.URL.Path, urlErr.Err)
		}
		return nil, fmt.Errorf("torbox: request %s %s failed: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("torbox: reading response: %w", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		if c.HTTP429Callback != nil {
			c.HTTP429Callback()
		}
	}

	if resp.StatusCode != http.StatusOK {
		// Log non-200 response bodies for diagnosis. TorBox usually includes
		// a detail/error message in the JSON body explaining why (e.g. expired
		// torrent, invalid file_id, rate limit, etc.).
		// Strip the token query param from the URL before logging to avoid
		// leaking the API key. url.Redacted() only masks the userinfo portion
		// of URLs, not query parameters.
		slog.Warn("torbox non-200 response",
			"status", resp.StatusCode,
			"endpoint", req.URL.Path,
			"body", truncateBody(body),
		)
		return nil, fmt.Errorf("torbox: unexpected status %d", resp.StatusCode)
	}

	return body, nil
}

// truncateBody truncates a response body to a reasonable length for logging.
func truncateBody(body []byte) string {
	const maxLen = 512
	if len(body) <= maxLen {
		return string(body)
	}
	return string(body[:maxLen]) + "... (truncated)"
}

// IsRetryable reports whether a TorBox API error is likely transient and
// worth retrying with exponential backoff. Returns false for nil errors.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "unexpected status 429") ||
		strings.Contains(s, "unexpected status 5") ||
		strings.Contains(s, "context deadline exceeded") ||
		strings.Contains(s, "Client.Timeout") ||
		strings.Contains(s, "invalid character '<'") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "no such host") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "EOF")
}