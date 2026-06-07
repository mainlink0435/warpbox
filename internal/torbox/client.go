// Package torbox provides a client for the TorBox API.
package torbox

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Client communicates with the TorBox API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a new TorBox API client.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// FileInfo represents a cached torrent file from TorBox.
type FileInfo struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	MimeType string `json:"mime_type"`
}

// ListFiles returns all files accessible via the TorBox API.
func (c *Client) ListFiles(ctx context.Context) ([]FileInfo, error) {
	// TODO: Implement TorBox /files endpoint.
	return nil, fmt.Errorf("torbox.ListFiles: not implemented")
}

// GetDownloadURL returns a secure CDN download URL for the given file.
func (c *Client) GetDownloadURL(ctx context.Context, fileID int) (string, error) {
	// TODO: Implement TorBox /request/dl endpoint.
	return "", fmt.Errorf("torbox.GetDownloadURL: not implemented")
}

// doRequest is a helper for authenticated HTTP requests to TorBox.
func (c *Client) doRequest(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	return c.httpClient.Do(req)
}