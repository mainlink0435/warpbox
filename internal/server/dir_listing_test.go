package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mainlink0435/warpbox/internal/metadata"
)

func TestServeDirListingRoot(t *testing.T) {
	// Open an in-memory store with some test data.
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory store: %v", err)
	}
	defer store.Close()

	// Seed some files.
	files := []metadata.FileRecord{
		{ItemID: 1, FileID: 10, Source: metadata.SourceTorrent, Name: "file1.mkv", Path: "Movie.A/file1.mkv", Size: 1000, MimeType: "video/x-matroska"},
		{ItemID: 1, FileID: 11, Source: metadata.SourceTorrent, Name: "file2.mkv", Path: "Movie.A/file2.mkv", Size: 2000, MimeType: "video/x-matroska"},
		{ItemID: 2, FileID: 20, Source: metadata.SourceTorrent, Name: "ep1.mkv",  Path: "Show.B/ep1.mkv", Size: 500, MimeType: "video/x-matroska"},
	}
	for _, f := range files {
		if err := store.UpsertFile(f); err != nil {
			t.Fatalf("failed to upsert file: %v", err)
		}
	}

	// Create a server pointing to the in-memory store.
	srv := New(Config{Version: "test"}, store, nil, nil)

	// Simulate GET /webdav/
	req := httptest.NewRequest(http.MethodGet, "/webdav/", nil)
	w := httptest.NewRecorder()
	srv.handleGet(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	// Verify status is 207 Multi-Status.
	if resp.StatusCode != http.StatusMultiStatus {
		t.Errorf("expected 207 Multi-Status, got %d %s", resp.StatusCode, resp.Status)
	}

	// Verify Content-Type is XML.
	ct := resp.Header.Get("Content-Type")
	if ct != "application/xml; charset=utf-8" {
		t.Errorf("expected XML content type, got %q", ct)
	}

	// Verify the DAV header is present.
	if resp.Header.Get("DAV") != "1" {
		t.Errorf("expected DAV: 1 header")
	}

	// Verify the XML is well-formed and contains expected elements.
	body := readAllStr(resp.Body)
	if !strings.Contains(body, "<D:multistatus") {
		t.Error("expected <D:multistatus> element")
	}
	if !strings.Contains(body, "<D:href>/webdav/</D:href>") {
		t.Error("expected root href /webdav/")
	}
	if !strings.Contains(body, "<D:collection>") {
		t.Error("expected collection element for directory")
	}
	if !strings.Contains(body, "__all__") {
		t.Error("expected __all__ synthetic directory in root response")
	}
	if strings.Contains(body, "Movie.A") || strings.Contains(body, "Show.B") {
		t.Error("root should NOT show real torrent dirs directly")
	}
}

func TestServeDirListingSubdir(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory store: %v", err)
	}
	defer store.Close()

	files := []metadata.FileRecord{
		{ItemID: 1, FileID: 10, Source: metadata.SourceTorrent, Name: "file1.mkv", Path: "Movie.A/file1.mkv", Size: 1000, MimeType: "video/x-matroska"},
	}
	for _, f := range files {
		if err := store.UpsertFile(f); err != nil {
			t.Fatalf("failed to upsert file: %v", err)
		}
	}

	srv := New(Config{Version: "test"}, store, nil, nil)

	// Simulate GET /webdav/Movie.A/ — a subdirectory.
	req := httptest.NewRequest(http.MethodGet, "/webdav/Movie.A/", nil)
	w := httptest.NewRecorder()
	srv.handleGet(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		t.Errorf("expected 207 Multi-Status, got %d %s", resp.StatusCode, resp.Status)
	}

	body := readAllStr(resp.Body)
	if !strings.Contains(body, "<D:multistatus") {
		t.Error("expected <D:multistatus> element")
	}
	if !strings.Contains(body, "<D:href>/webdav/Movie.A/</D:href>") {
		t.Error("expected dir href /webdav/Movie.A/")
	}
	if !strings.Contains(body, "<D:collection>") {
		t.Error("expected collection element for directory")
	}
	if !strings.Contains(body, "file1.mkv") {
		t.Error("expected file1.mkv in response")
	}
}

func TestServeDirListingMissingPath(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory store: %v", err)
	}
	defer store.Close()

	srv := New(Config{Version: "test"}, store, nil, nil)

	// GET on a path that doesn't exist and has no children.
	req := httptest.NewRequest(http.MethodGet, "/webdav/nonexistent", nil)
	w := httptest.NewRecorder()
	srv.handleGet(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for nonexistent path, got %d", resp.StatusCode)
	}
}

func readAllStr(r io.ReadCloser) string {
	b, _ := io.ReadAll(r)
	r.Close()
	return string(b)
}

func TestServeDirListingNestedPaths(t *testing.T) {
	// Test that PROPFIND with depth=1 on a directory containing files
	// with nested paths returns only immediate children, not deeply nested entries.
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory store: %v", err)
	}
	defer store.Close()

	// Simulate the real-world scenario from issue #37:
	// Torrent "The.Studio.2025.S01.MULTi.1080p.WEB.H265-FW" contains files with
	// paths like "The.Studio.2025.S01E02.MULTi.1080p.WEB.H265-FW/The.Studio.2025.S01E02.MULTi.1080p.WEB.H265-FW.mkv"
	// This creates a nested structure: TorrentName/SubDir/File.ext
	files := []metadata.FileRecord{
		// A torrent with nested subdirectories (the common case from the bug report)
		{ItemID: 1, FileID: 10, Source: metadata.SourceTorrent, Name: "The.Studio.2025.S01E02.MULTi.1080p.WEB.H265-FW.mkv",
			Path: "The.Studio.2025.S01.MULTi.1080p.WEB.H265-FW/The.Studio.2025.S01E02.MULTi.1080p.WEB.H265-FW/The.Studio.2025.S01E02.MULTi.1080p.WEB.H265-FW.mkv",
			Size: 1000, MimeType: "video/x-matroska"},
		{ItemID: 1, FileID: 11, Source: metadata.SourceTorrent, Name: "The.Studio.2025.S01E02.MULTi.1080p.WEB.H265-FW.nfo",
			Path: "The.Studio.2025.S01.MULTi.1080p.WEB.H265-FW/The.Studio.2025.S01E02.MULTi.1080p.WEB.H265-FW/The.Studio.2025.S01E02.MULTi.1080p.WEB.H265-FW.nfo",
			Size: 500, MimeType: "text/plain"},
		// A torrent where files are directly at the root (normal case)
		{ItemID: 2, FileID: 20, Source: metadata.SourceTorrent, Name: "movie.mkv",
			Path: "Simple.Movie/movie.mkv",
			Size: 2000, MimeType: "video/x-matroska"},
	}
	for _, f := range files {
		if err := store.UpsertFile(f); err != nil {
			t.Fatalf("failed to upsert file: %v", err)
		}
	}

	srv := New(Config{Version: "test"}, store, nil, nil)

	// --- Test 1: Listing the root should show only the __all__ synthetic directory ---
	req := httptest.NewRequest(http.MethodGet, "/webdav/", nil)
	w := httptest.NewRecorder()
	srv.handleGet(w, req)

	resp := w.Result()
	body := readAllStr(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		t.Errorf("expected 207 Multi-Status, got %d", resp.StatusCode)
	}

	// Should NOT contain deeply nested paths as direct children
	if strings.Contains(body, "The.Studio.2025.S01E02.MULTi.1080p.WEB.H265-FW.mkv") {
		t.Error("root listing should NOT contain deeply nested file; only the torrent directory")
	}
	if strings.Contains(body, "Simple.Movie/movie.mkv") {
		t.Error("root listing should NOT contain child file paths with slash")
	}

	// Root should only show __all__ synthetic dir, not real torrent names
	if !strings.Contains(body, "__all__") {
		t.Error("root listing should contain __all__ synthetic directory")
	}
	if strings.Contains(body, "<D:href>/webdav/The.Studio.2025.S01.MULTi.1080p.WEB.H265-FW/</D:href>") {
		t.Error("root listing should NOT contain torrent dirs directly; only __all__")
	}
	if strings.Contains(body, "<D:href>/webdav/Simple.Movie/</D:href>") {
		t.Error("root listing should NOT contain torrent dirs directly; only __all__")
	}



	// --- Test 2: Listing the Simple.Movie directory should show the file directly ---
	req2 := httptest.NewRequest(http.MethodGet, "/webdav/Simple.Movie/", nil)
	w2 := httptest.NewRecorder()
	srv.handleGet(w2, req2)

	resp2 := w2.Result()
	body2 := readAllStr(resp2.Body)
	resp2.Body.Close()

	if !strings.Contains(body2, "<D:href>/webdav/Simple.Movie/movie.mkv</D:href>") {
		t.Error("Simple.Movie listing should contain movie.mkv directly")
	}

	// --- Test 3: Listing the nested torrent directory should show the subdirectory, not the file ---
	req3 := httptest.NewRequest(http.MethodGet, "/webdav/The.Studio.2025.S01.MULTi.1080p.WEB.H265-FW/", nil)
	w3 := httptest.NewRecorder()
	srv.handleGet(w3, req3)

	resp3 := w3.Result()
	body3 := readAllStr(resp3.Body)
	resp3.Body.Close()

	// Should contain the subdirectory entry (with trailing slash)
	if !strings.Contains(body3, "<D:href>/webdav/The.Studio.2025.S01.MULTi.1080p.WEB.H265-FW/The.Studio.2025.S01E02.MULTi.1080p.WEB.H265-FW/</D:href>") {
		t.Error("nested torrent listing should contain subdirectory, not deeply nested file")
	}
	// Should NOT contain the deeply nested file directly in this listing
	if strings.Contains(body3, "The.Studio.2025.S01E02.MULTi.1080p.WEB.H265-FW.mkv") {
		t.Error("nested torrent listing should NOT contain the file directly; only the subdirectory")
	}

	// Should have a <D:collection> for the subdirectory
	if !strings.Contains(body3, "<D:collection>") {
		t.Error("subdirectory should have a collection resource type")
	}
}

// TestServeDirListingPercentInFilename ensures files with a literal '%' in the
// name emit valid percent-encoded D:href values (rclone URL-join fix) while
// keeping human-readable displaynames and successful GET path lookup.
func TestServeDirListingPercentInFilename(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory store: %v", err)
	}
	defer store.Close()

	name := "Futurama_S04E11_The 30% Iron Chef.mp4"
	path := "Futurama.S04/" + name
	if err := store.UpsertFile(metadata.FileRecord{
		ItemID: 1, FileID: 10, Source: metadata.SourceTorrent,
		Name: name, Path: path, Size: 1234, MimeType: "video/mp4",
	}); err != nil {
		t.Fatalf("failed to upsert file: %v", err)
	}

	srv := New(Config{Version: "test"}, store, nil, nil)

	// PROPFIND via GET directory listing.
	req := httptest.NewRequest(http.MethodGet, "/webdav/Futurama.S04/", nil)
	w := httptest.NewRecorder()
	srv.handleGet(w, req)
	resp := w.Result()
	body := readAllStr(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("expected 207, got %d", resp.StatusCode)
	}

	// Wire href must encode '%' as %25 (and space as %20).
	wantHref := "<D:href>/webdav/Futurama.S04/Futurama_S04E11_The%2030%25%20Iron%20Chef.mp4</D:href>"
	if !strings.Contains(body, wantHref) {
		t.Errorf("expected encoded href %s\nbody:\n%s", wantHref, body)
	}
	// Must not emit an unencoded href (invalid URL escape for clients like rclone).
	badHref := "<D:href>/webdav/Futurama.S04/Futurama_S04E11_The 30% Iron Chef.mp4</D:href>"
	if strings.Contains(body, badHref) {
		t.Error("must not emit unencoded href with literal %")
	}
	// Display name keeps the literal percent for clients/UI.
	if !strings.Contains(body, "<D:displayname>"+name+"</D:displayname>") {
		t.Errorf("expected displayname with literal %%: %q", name)
	}

	// HEAD with a properly encoded URL path: Go decodes to the DB path with a
	// literal '%'. Must not 404 (lookup works regardless of CDN availability).
	headReq := httptest.NewRequest(http.MethodHead, "/webdav/Futurama.S04/Futurama_S04E11_The%2030%25%20Iron%20Chef.mp4", nil)
	headW := httptest.NewRecorder()
	srv.handleHead(headW, headReq)
	headResp := headW.Result()
	headResp.Body.Close()
	if headResp.StatusCode == http.StatusNotFound {
		t.Errorf("HEAD encoded path should find file, got %d", headResp.StatusCode)
	}
}

func TestServeDirListingGETRootNoSlash(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatalf("failed to open in-memory store: %v", err)
	}
	defer store.Close()

	files := []metadata.FileRecord{
		{ItemID: 1, FileID: 10, Source: metadata.SourceTorrent, Name: "file.mkv", Path: "Torrent/file.mkv", Size: 1000, MimeType: "video/x-matroska"},
	}
	for _, f := range files {
		if err := store.UpsertFile(f); err != nil {
			t.Fatalf("failed to upsert file: %v", err)
		}
	}

	srv := New(Config{Version: "test"}, store, nil, nil)

	// GET /webdav (without trailing slash) — this is the case the user reported.
	req := httptest.NewRequest(http.MethodGet, "/webdav", nil)
	w := httptest.NewRecorder()
	srv.handleGet(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMultiStatus {
		t.Errorf("expected 207 Multi-Status, got %d", resp.StatusCode)
	}

	body := readAllStr(resp.Body)
	if !strings.Contains(body, "<D:multistatus") {
		t.Error("expected valid multi-status XML")
	}
}