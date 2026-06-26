package torbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func newTestClient(serverURL, apiKey string) *Client {
	c := NewClient(apiKey)
	c.baseURL = serverURL
	return c
}

func TestListFilesSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("expected Bearer token, got %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/v1/api/torrents/mylist" {
			t.Errorf("expected /v1/api/torrents/mylist, got %s", r.URL.Path)
		}

		resp := apiResponse[[]Torrent]{
			Data: []Torrent{
				{
					ID:   1,
					Name: "Test Torrent",
					Hash: "abc123",
					Size: 1000,
					DownloadState: "cached",
					Files: []TorrentFile{
						{ID: 10, Name: "movie.mkv", Size: 500, MimeType: "video/x-matroska"},
						{ID: 11, Name: "subs.srt", Size: 50, MimeType: "text/plain"},
					},
				},
			},
			Success: boolPtr(true),
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := newTestClient(server.URL, "test-key")
	torrents, err := client.ListTorrents(context.Background(), ListFilesParams{})
	if err != nil {
		t.Fatalf("ListTorrents failed: %v", err)
	}
	if len(torrents) != 1 {
		t.Fatalf("expected 1 torrent, got %d", len(torrents))
	}
	if torrents[0].Name != "Test Torrent" {
		t.Errorf("name = %q", torrents[0].Name)
	}
	if len(torrents[0].Files) != 2 {
		t.Errorf("expected 2 files, got %d", len(torrents[0].Files))
	}
}

func TestListFilesAuthError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"data":null,"success":false,"detail":"Invalid API key"}`))
	}))
	defer server.Close()

	client := newTestClient(server.URL, "bad-key")
	_, err := client.ListTorrents(context.Background(), ListFilesParams{})
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
}

func TestListFilesRateLimited(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"data":null,"success":false,"detail":"Rate limited"}`))
	}))
	defer server.Close()

	client := newTestClient(server.URL, "key")
	_, err := client.ListTorrents(context.Background(), ListFilesParams{})
	if err == nil {
		t.Fatal("expected error for 429, got nil")
	}
}

func TestListFilesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := newTestClient(server.URL, "key")
	_, err := client.ListTorrents(context.Background(), ListFilesParams{})
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}
}

func TestGetDownloadURLSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/torrents/requestdl" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("token") != "test-key" {
			t.Errorf("expected token param, got %q", r.URL.Query().Get("token"))
		}
		if r.URL.Query().Get("torrent_id") != "42" {
			t.Errorf("expected torrent_id=42, got %s", r.URL.Query().Get("torrent_id"))
		}
		if r.URL.Query().Get("file_id") != "7" {
			t.Errorf("expected file_id=7, got %s", r.URL.Query().Get("file_id"))
		}

		resp := apiResponse[string]{
			Data:    "https://cdn.torbox.app/dl/abc123",
			Success: boolPtr(true),
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := newTestClient(server.URL, "test-key")
	url, err := client.GetDownloadURL(context.Background(), 42, 7, false)
	if err != nil {
		t.Fatalf("GetDownloadURL failed: %v", err)
	}
	if url != "https://cdn.torbox.app/dl/abc123" {
		t.Errorf("got %q, want %q", url, "https://cdn.torbox.app/dl/abc123")
	}
}

func TestGetUsenetDownloadURLSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/usenet/requestdl" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("token") != "test-key" {
			t.Errorf("expected token param, got %q", r.URL.Query().Get("token"))
		}
		if r.URL.Query().Get("usenet_id") != "1644029" {
			t.Errorf("expected usenet_id=1644029, got %s", r.URL.Query().Get("usenet_id"))
		}
		if r.URL.Query().Get("file_id") != "7" {
			t.Errorf("expected file_id=7, got %s", r.URL.Query().Get("file_id"))
		}
		if r.URL.Query().Get("torrent_id") != "" {
			t.Errorf("unexpected torrent_id param for usenet endpoint: %s", r.URL.Query().Get("torrent_id"))
		}

		resp := apiResponse[string]{
			Data:    "https://cdn.torbox.app/usenet/dl/xyz789",
			Success: boolPtr(true),
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := newTestClient(server.URL, "test-key")
	url, err := client.GetUsenetDownloadURL(context.Background(), 1644029, 7, false)
	if err != nil {
		t.Fatalf("GetUsenetDownloadURL failed: %v", err)
	}
	if url != "https://cdn.torbox.app/usenet/dl/xyz789" {
		t.Errorf("got %q, want %q", url, "https://cdn.torbox.app/usenet/dl/xyz789")
	}
}

func TestGetUsenetDownloadURLEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/api/usenet/requestdl" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("usenet_id") != "5" {
			t.Errorf("expected usenet_id=5, got %s", r.URL.Query().Get("usenet_id"))
		}
		resp := apiResponse[string]{Data: "", Success: boolPtr(true)}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := newTestClient(server.URL, "key")
	url, err := client.GetUsenetDownloadURL(context.Background(), 5, 1, false)
	if err != nil {
		t.Fatalf("GetUsenetDownloadURL failed: %v", err)
	}
	if url != "" {
		t.Errorf("expected empty URL, got %q", url)
	}
}

func TestClientRecoversAfterErrors(t *testing.T) {
	// Simulate a server that returns error then success.
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount <= 2 {
			// First two calls fail.
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"data":null,"success":false,"detail":"Rate limited"}`))
			return
		}
		// Third call succeeds.
		resp := apiResponse[[]Torrent]{
			Data: []Torrent{
				{ID: 1, Name: "Recovered Torrent", Hash: "abc", Size: 500,
					DownloadState: "cached",
					Files: []TorrentFile{
						{ID: 10, Name: "recovered.mkv", Size: 500, MimeType: "video/x-matroska"},
					},
				},
			},
			Success: boolPtr(true),
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := newTestClient(server.URL, "test-key")

	// First call should fail with 429.
	_, err1 := client.ListTorrents(context.Background(), ListFilesParams{})
	if err1 == nil {
		t.Fatal("expected error on first call (429), got nil")
	}

	// Second call should also fail with 429.
	_, err2 := client.ListTorrents(context.Background(), ListFilesParams{})
	if err2 == nil {
		t.Fatal("expected error on second call (429), got nil")
	}

	// Third call should succeed.
	torrents, err3 := client.ListTorrents(context.Background(), ListFilesParams{})
	if err3 != nil {
		t.Fatalf("expected success on third call, got: %v", err3)
	}
	if len(torrents) != 1 {
		t.Fatalf("expected 1 torrent, got %d", len(torrents))
	}
	if torrents[0].Name != "Recovered Torrent" {
		t.Errorf("name = %q", torrents[0].Name)
	}
	if callCount != 3 {
		t.Errorf("expected 3 calls to mock server, got %d", callCount)
	}
}

func TestGetDownloadURLEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := apiResponse[string]{Data: "", Success: boolPtr(true)}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := newTestClient(server.URL, "key")
	url, err := client.GetDownloadURL(context.Background(), 1, 1, false)
	if err != nil {
		t.Fatalf("GetDownloadURL failed: %v", err)
	}
	if url != "" {
		t.Errorf("expected empty URL, got %q", url)
	}
}

type errorTransport struct {
	err error
}

func (t *errorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

func TestDoSanitizesURLFromNetworkError(t *testing.T) {
	// Use a transport that returns a plain error. http.Client.Do() wraps it
	// in *url.Error, which our sanitization should strip — verify the
	// error string does not leak the API token query parameter.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server should never be reached — transport mock returns error first.
		t.Error("unexpected server call")
	}))
	defer server.Close()

	client := newTestClient(server.URL, "secret-key-12345")
	// Override the HTTP client with one that fails on every request.
	// Return a plain error (not a *url.Error) because http.Client.Do()
	// wraps any transport error in its own *url.Error containing the full
	// request URL with token query parameter.
	client.httpClient = &http.Client{
		Transport: &errorTransport{
			err: fmt.Errorf("dial tcp: connection refused"),
		},
	}

	_, err := client.GetDownloadURL(context.Background(), 1, 1, false)
	if err == nil {
		t.Fatal("expected error from broken transport, got nil")
	}

	errStr := err.Error()
	if !strings.Contains(errStr, "/v1/api/torrents/requestdl") {
		t.Errorf("error should contain sanitized path, got: %s", errStr)
	}
	if strings.Contains(errStr, "secret-key-12345") {
		t.Errorf("error must NOT contain API token, got: %s", errStr)
	}
	if strings.Contains(errStr, "token=") {
		t.Errorf("error must NOT contain 'token=' query param, got: %s", errStr)
	}
}

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"429 rate limit", fmt.Errorf("torbox: unexpected status 429"), true},
		{"500 server error", fmt.Errorf("torbox: unexpected status 500"), true},
		{"502 bad gateway", fmt.Errorf("torbox: unexpected status 502"), true},
		{"503 unavailable", fmt.Errorf("torbox: unexpected status 503"), true},
		{"504 gateway timeout", fmt.Errorf("torbox: unexpected status 504"), true},
		{"context deadline exceeded", fmt.Errorf("torbox: request GET /v1/api/usenet/mylist failed: context deadline exceeded (Client.Timeout exceeded while awaiting headers)"), true},
		{"Client.Timeout", fmt.Errorf("torbox: request GET failed: Client.Timeout"), true},
		{"HTML response", fmt.Errorf("torbox: decoding torrents response: invalid character '<' looking for beginning of value"), true},
		{"connection refused", fmt.Errorf("torbox: request GET failed: dial tcp: connection refused"), true},
		{"no such host", fmt.Errorf("torbox: request GET failed: dial tcp: lookup api.torbox.app: no such host"), true},
		{"i/o timeout", fmt.Errorf("torbox: request GET failed: dial tcp: i/o timeout"), true},
		{"EOF", fmt.Errorf("torbox: request GET failed: unexpected EOF"), true},
		{"401 unauthorized", fmt.Errorf("torbox: unexpected status 401"), false},
		{"404 not found", fmt.Errorf("torbox: unexpected status 404"), false},
		{"400 bad request", fmt.Errorf("torbox: unexpected status 400"), false},
		{"API-level error", fmt.Errorf("torbox torrents API error: invalid api_key"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRetryable(tt.err); got != tt.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestHTMLResponseCausesJSONError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// TorBox sometimes returns Cloudflare HTML pages with HTTP 200.
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`<html><head><title>502 Bad Gateway</title></head><body>Cloudflare error</body></html>`))
	}))
	defer server.Close()

	client := newTestClient(server.URL, "key")
	_, err := client.ListTorrents(context.Background(), ListFilesParams{})
	if err == nil {
		t.Fatal("expected error for HTML response with status 200, got nil")
	}
	if !strings.Contains(err.Error(), "invalid character '<'") {
		t.Errorf("error should mention invalid character '<', got: %v", err)
	}
	if !IsRetryable(err) {
		t.Errorf("HTML response error should be retryable, got: %v", err)
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func TestListFilesWithParams(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pagination starts at params.Offset and requests a fixed page window
		// (defaultListPageSize); params.Limit is now a TOTAL ceiling, not the
		// per-request limit.
		if r.URL.Query().Get("offset") != "10" {
			t.Errorf("expected offset=10, got %s", r.URL.Query().Get("offset"))
		}
		if r.URL.Query().Get("limit") != strconv.Itoa(defaultListPageSize) {
			t.Errorf("expected limit=%d (page size), got %s", defaultListPageSize, r.URL.Query().Get("limit"))
		}
		if r.URL.Query().Get("bypass_cache") != "true" {
			t.Errorf("expected bypass_cache=true, got %s", r.URL.Query().Get("bypass_cache"))
		}
		resp := apiResponse[[]Torrent]{Data: []Torrent{}, Success: boolPtr(true)}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := newTestClient(server.URL, "key")
	_, err := client.ListTorrents(context.Background(), ListFilesParams{
		BypassCache: true,
		Offset:      10,
		Limit:       50,
	})
	if err != nil {
		t.Fatalf("ListTorrents failed: %v", err)
	}
}

// TestListPaginatesPastCap locks in the fix for TorBox's ~10k per-response cap:
// listGeneric must page through with offset until a short page ends the list,
// accumulating ALL items, not just the first page. A full page (pageSize)
// signals "more follow"; anything shorter is the last page.
func TestListPaginatesPastCap(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var data []Torrent
		switch r.URL.Query().Get("offset") {
		case "0":
			data = make([]Torrent, defaultListPageSize) // full page → more follow
		case strconv.Itoa(defaultListPageSize):
			data = make([]Torrent, 7) // short page → end
		default:
			t.Errorf("unexpected offset %q", r.URL.Query().Get("offset"))
		}
		json.NewEncoder(w).Encode(apiResponse[[]Torrent]{Data: data, Success: boolPtr(true)})
	}))
	defer server.Close()

	client := newTestClient(server.URL, "key")
	got, err := client.ListTorrents(context.Background(), ListFilesParams{Limit: 50000})
	if err != nil {
		t.Fatalf("ListTorrents failed: %v", err)
	}
	if want := defaultListPageSize + 7; len(got) != want {
		t.Errorf("got %d items, want %d (pagination must accumulate all pages)", len(got), want)
	}
	if calls != 2 {
		t.Errorf("expected 2 page requests, got %d", calls)
	}
}