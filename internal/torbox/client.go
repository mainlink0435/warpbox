// Package torbox provides a client for the TorBox API.
//
// Implementation based on the official OpenAPI spec at
// https://api.torbox.app/openapi.json
package torbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// Client communicates with the TorBox API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a new TorBox API client.
// baseURL should be e.g. "https://api.torbox.app"
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
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

// ListFilesParams are optional query parameters for GetTorrentList.
type ListFilesParams struct {
	BypassCache bool
	Offset      int
	Limit       int
}

// ListFiles returns all torrents and their files from the user's TorBox account.
func (c *Client) ListFiles(ctx context.Context, params ListFilesParams) ([]Torrent, error) {
	u, _ := url.Parse(c.baseURL + "/v1/api/torrents/mylist")
	q := u.Query()
	q.Set("bypass_cache", strconv.FormatBool(params.BypassCache))
	q.Set("offset", strconv.Itoa(params.Offset))
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("torbox: creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	slog.Debug("torbox.ListFiles", "offset", params.Offset, "limit", params.Limit)

	body, err := c.do(req)
	if err != nil {
		return nil, err
	}

	var env apiResponse[[]Torrent]
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("torbox: decoding response: %w", err)
	}

	if env.Error != nil && *env.Error != "" {
		return nil, fmt.Errorf("torbox API error: %s", *env.Error)
	}

	n := len(env.Data)
	slog.Debug("torbox.ListFiles result", "torrents", n)
	return env.Data, nil
}

// ---------------------------------------------------------------------------
// GetDownloadURL
// ---------------------------------------------------------------------------

// GetDownloadURL returns a CDN download URL for the given file in a torrent.
// The returned URL expires after a few hours but is renewable.
func (c *Client) GetDownloadURL(ctx context.Context, torrentID, fileID int64, redirect bool) (string, error) {
	u, _ := url.Parse(c.baseURL + "/v1/api/torrents/requestdl")
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

	slog.Debug("torbox.GetDownloadURL", "torrent_id", torrentID, "file_id", fileID)

	body, err := c.do(req)
	if err != nil {
		return "", err
	}

	var env apiResponse[string]
	if err := json.Unmarshal(body, &env); err != nil {
		return "", fmt.Errorf("torbox: decoding response: %w", err)
	}

	if env.Error != nil && *env.Error != "" {
		return "", fmt.Errorf("torbox API error: %s", *env.Error)
	}

	slog.Debug("torbox.GetDownloadURL result", "has_url", env.Data != "")
	return env.Data, nil
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

// do executes an HTTP request, reads the full body, and returns the body bytes.
// The response body is always closed before returning.
func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("torbox: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("torbox: reading response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("torbox: authentication failed (401)")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("torbox: rate limited (429)")
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("torbox: server error (%d)", resp.StatusCode)
	}

	return body, nil
}