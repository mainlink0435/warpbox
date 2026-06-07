package torbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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

	client := NewClient(server.URL, "test-key")
	torrents, err := client.ListFiles(context.Background(), ListFilesParams{})
	if err != nil {
		t.Fatalf("ListFiles failed: %v", err)
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

	client := NewClient(server.URL, "bad-key")
	_, err := client.ListFiles(context.Background(), ListFilesParams{})
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

	client := NewClient(server.URL, "key")
	_, err := client.ListFiles(context.Background(), ListFilesParams{})
	if err == nil {
		t.Fatal("expected error for 429, got nil")
	}
}

func TestListFilesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient(server.URL, "key")
	_, err := client.ListFiles(context.Background(), ListFilesParams{})
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

	client := NewClient(server.URL, "test-key")
	url, err := client.GetDownloadURL(context.Background(), 42, 7, false)
	if err != nil {
		t.Fatalf("GetDownloadURL failed: %v", err)
	}
	if url != "https://cdn.torbox.app/dl/abc123" {
		t.Errorf("got %q, want %q", url, "https://cdn.torbox.app/dl/abc123")
	}
}

func TestGetDownloadURLEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := apiResponse[string]{Data: "", Success: boolPtr(true)}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL, "key")
	url, err := client.GetDownloadURL(context.Background(), 1, 1, false)
	if err != nil {
		t.Fatalf("GetDownloadURL failed: %v", err)
	}
	if url != "" {
		t.Errorf("expected empty URL, got %q", url)
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func TestListFilesWithParams(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("offset") != "10" {
			t.Errorf("expected offset=10, got %s", r.URL.Query().Get("offset"))
		}
		if r.URL.Query().Get("limit") != "50" {
			t.Errorf("expected limit=50, got %s", r.URL.Query().Get("limit"))
		}
		if r.URL.Query().Get("bypass_cache") != "true" {
			t.Errorf("expected bypass_cache=true, got %s", r.URL.Query().Get("bypass_cache"))
		}
		resp := apiResponse[[]Torrent]{Data: []Torrent{}, Success: boolPtr(true)}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL, "key")
	_, err := client.ListFiles(context.Background(), ListFilesParams{
		BypassCache: true,
		Offset:      10,
		Limit:       50,
	})
	if err != nil {
		t.Fatalf("ListFiles failed: %v", err)
	}
}