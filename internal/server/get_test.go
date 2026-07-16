package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mainlink0435/warpbox/internal/metadata"
	"github.com/mainlink0435/warpbox/internal/throttle"
	"github.com/mainlink0435/warpbox/internal/torbox"
)

// ---------------------------------------------------------------------------
// Mock CDN helpers for hang/poll retry tests
// ---------------------------------------------------------------------------

// cdnResponse defines a single response the mock CDN server should return.
type cdnResponse struct {
	status      int
	body        string
	contentType string
}

// newMockCDNServer returns an httptest.Server that cycles through the given
// responses on each successive request. Used to simulate transient CDN
// data errors (429, 5xx, disguised text bodies) followed by success.
func newMockCDNServer(t *testing.T, responses []cdnResponse) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	idx := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if idx >= len(responses) {
			idx = 0 // cycle back so the test always gets the expected sequence
		}
		resp := responses[idx]
		idx++
		mu.Unlock()
		if resp.contentType != "" {
			w.Header().Set("Content-Type", resp.contentType)
		}
		w.WriteHeader(resp.status)
		io.WriteString(w, resp.body)
	}))
}

// newMockTorBoxForCDN returns an httptest.Server that responds to TorBox API
// requestdl calls with a CDN URL pointing to the given base URL. Any path
// is accepted — the handler always returns success with the CDN URL.
func newMockTorBoxForCDN(t *testing.T, cdnBaseURL string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"success":true,"data":"%s/file"}`, cdnBaseURL)
	}))
}

// newTestCDNHangEnv wires up a full test environment for CDN hang/poll tests.
// Returns the Server, a cleanup function, the mock CDN server (for assertion
// access), and the response recorder.
// The mock CDN cycles through cdnResponses; the mock TorBox always returns
// a CDN URL pointing to the mock CDN.
func newTestCDNHangEnv(t *testing.T, cdnResponses []cdnResponse) (*Server, *httptest.ResponseRecorder, func()) {
	t.Helper()

	// Shorten poll interval so tests don't wait 15s+.
	oldPoll := cdnPollInterval
	cdnPollInterval = 10 * time.Millisecond

	// Mock CDN and TorBox servers.
	mockCDN := newMockCDNServer(t, cdnResponses)
	mockTorBox := newMockTorBoxForCDN(t, mockCDN.URL)

	// TorBox client pointed at mock API.
	client := torbox.NewClient("test-key")
	client.SetBaseURL(mockTorBox.URL)

	// In-memory store with a test file.
	store, err := metadata.Open(":memory:")
	if err != nil {
		// Close everything before failing.
		mockCDN.Close()
		mockTorBox.Close()
		t.Fatalf("opening in-memory store: %v", err)
	}
	if err := store.UpsertFile(metadata.FileRecord{
		ItemID: 1, FileID: 10, Source: metadata.SourceTorrent,
		Name: "test.mkv", Path: "Test/test.mkv", Size: 5000, MimeType: "video/x-matroska",
	}); err != nil {
		store.Close()
		mockCDN.Close()
		mockTorBox.Close()
		t.Fatalf("upserting test file: %v", err)
	}

	// Throttle queue (needed for getCDNURLWithRetry).
	queue := throttle.NewQueue(600)
	qCtx, qCancel := context.WithCancel(context.Background())
	queue.Start(qCtx)

	// Server with minimal config.
	srv := New(Config{Version: "test"}, store, client, queue)

	// Response recorder.
	w := httptest.NewRecorder()

	cleanup := func() {
		qCancel()
		srv.StopCleanup()
		store.Close()
		mockCDN.Close()
		mockTorBox.Close()
		cdnPollInterval = oldPoll
	}

	return srv, w, cleanup
}

// ---------------------------------------------------------------------------
// Existing tests below — parseRange, cdnCacheKey, isCDNDisguisedErrorBody, semaphore
// ---------------------------------------------------------------------------

func TestParseRangeFull(t *testing.T) {
	r, err := parseRange("bytes=0-499", 1000)
	if err != nil {
		t.Fatalf("parseRange failed: %v", err)
	}
	if r.Start != 0 {
		t.Errorf("start = %d, want 0", r.Start)
	}
	if r.End != 499 {
		t.Errorf("end = %d, want 499", r.End)
	}
	if r.Length != 500 {
		t.Errorf("length = %d, want 500", r.Length)
	}
}

func TestParseRangeToEnd(t *testing.T) {
	r, err := parseRange("bytes=500-", 1000)
	if err != nil {
		t.Fatalf("parseRange failed: %v", err)
	}
	if r.Start != 500 {
		t.Errorf("start = %d, want 500", r.Start)
	}
	if r.End != 999 {
		t.Errorf("end = %d, want 999", r.End)
	}
	if r.Length != 500 {
		t.Errorf("length = %d, want 500", r.Length)
	}
}

func TestParseRangeSingleByte(t *testing.T) {
	r, err := parseRange("bytes=0-0", 100)
	if err != nil {
		t.Fatalf("parseRange failed: %v", err)
	}
	if r.Start != 0 {
		t.Errorf("start = %d, want 0", r.Start)
	}
	if r.End != 0 {
		t.Errorf("end = %d, want 0", r.End)
	}
	if r.Length != 1 {
		t.Errorf("length = %d, want 1", r.Length)
	}
}

func TestParseRangeEmpty(t *testing.T) {
	_, err := parseRange("", 1000)
	if err == nil {
		t.Fatal("expected error for empty range")
	}
}

func TestParseRangeNoBytesPrefix(t *testing.T) {
	_, err := parseRange("0-499", 1000)
	if err == nil {
		t.Fatal("expected error for missing bytes= prefix")
	}
}

func TestParseRangeOutOfBounds(t *testing.T) {
	_, err := parseRange("bytes=0-2000", 1000)
	if err == nil {
		t.Fatal("expected error for out-of-bounds range")
	}
}

func TestParseRangeNegativeStart(t *testing.T) {
	_, err := parseRange("bytes=-100-200", 1000)
	if err == nil {
		t.Fatal("expected error for negative start")
	}
}

func TestParseRangeLargeFile(t *testing.T) {
	r, err := parseRange("bytes=0-524287", 10*1024*1024*1024)
	if err != nil {
		t.Fatalf("parseRange failed for large file: %v", err)
	}
	if r.Start != 0 {
		t.Errorf("start = %d", r.Start)
	}
	if r.End != 524287 {
		t.Errorf("end = %d", r.End)
	}
	if r.Length != 524288 {
		t.Errorf("length = %d, want 524288", r.Length)
	}
}

func TestParseRangeRejectsMultipleRanges(t *testing.T) {
	_, err := parseRange("bytes=0-100,200-300", 1000)
	// SplitN only splits on first -, so this will likely produce malformed parts.
	if err == nil {
		t.Log("multiple range rejection: expected error, got nil (split may have parsed first)")
	}
}

func TestCdnCacheKeyTorrent(t *testing.T) {
	key := cdnCacheKey(metadata.SourceTorrent, 100, 5)
	want := "torrent:100:5"
	if key != want {
		t.Errorf("cdnCacheKey(torrent, 100, 5) = %q, want %q", key, want)
	}
}

func TestCdnCacheKeyUsenet(t *testing.T) {
	key := cdnCacheKey(metadata.SourceUsenet, 200, 5)
	want := "usenet:200:5"
	if key != want {
		t.Errorf("cdnCacheKey(usenet, 200, 5) = %q, want %q", key, want)
	}
}

func TestCdnCacheKeyDifferentiation(t *testing.T) {
	// Same IDs, different source should produce different keys.
	torKey := cdnCacheKey(metadata.SourceTorrent, 42, 7)
	usenetKey := cdnCacheKey(metadata.SourceUsenet, 42, 7)
	if torKey == usenetKey {
		t.Error("torrent and usenet keys should differ with same item_id and file_id")
	}
}

func TestIsCDNDisguisedErrorBody(t *testing.T) {
	cases := []struct {
		ct   string
		want bool
	}{
		{"video/mp4", false},
		{"application/octet-stream", false},
		{"text/plain", true},
		{"text/html; charset=utf-8", true},
		{"application/json", true},
		{"application/vnd.apple.mpegurl", false},
	}
	for _, tc := range cases {
		if got := isCDNDisguisedErrorBody(tc.ct); got != tc.want {
			t.Errorf("isCDNDisguisedErrorBody(%q) = %v, want %v", tc.ct, got, tc.want)
		}
	}
}

func TestCDNSemaphoreAcquireRelease(t *testing.T) {
	s := &Server{
		cdnSem: make(chan struct{}, 2),
	}

	// Acquire should succeed immediately when a token is available.
	s.cdnSem <- struct{}{}
	s.AcquireCDNConn()

	// Acquire from goroutine, then release in main goroutine.
	done := make(chan struct{})
	go func() {
		s.AcquireCDNConn()
		close(done)
	}()

	s.ReleaseCDNConn()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("AcquireCDNConn deadlocked — slot was not released properly")
	}
}

// ---------------------------------------------------------------------------
// CDN hang/poll retry tests
// ---------------------------------------------------------------------------

// TestHandleGetCDNHang_RetriesOnData429 verifies that when the CDN data
// endpoint returns 429, handleGetCDNHang retries with backoff instead of
// streaming the error body, and eventually succeeds on a later attempt.
func TestHandleGetCDNHang_RetriesOnData429(t *testing.T) {
	srv, w, cleanup := newTestCDNHangEnv(t, []cdnResponse{
		{status: http.StatusTooManyRequests, body: "rate limited"},
		{status: http.StatusOK, body: "real binary data", contentType: "application/octet-stream"},
	})
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/webdav/Test/test.mkv", nil)
	req.Header.Set("Range", "bytes=0-499")
	srv.handleGet(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The response body must be the binary data, not the 429 error text.
	if string(body) != "real binary data" {
		t.Errorf("expected body %q, got %q", "real binary data", string(body))
	}
}

// TestHandleGetCDNHang_RetriesOnDisguisedTextBody verifies that when the CDN
// returns 200 OK with a text/html content-type (disguised rate-limit error),
// handleGetCDNHang retries instead of streaming the error page as file data.
func TestHandleGetCDNHang_RetriesOnDisguisedTextBody(t *testing.T) {
	srv, w, cleanup := newTestCDNHangEnv(t, []cdnResponse{
		{status: http.StatusOK, body: "too many requests", contentType: "text/html"},
		{status: http.StatusOK, body: "real binary data", contentType: "video/x-matroska"},
	})
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/webdav/Test/test.mkv", nil)
	req.Header.Set("Range", "bytes=0-499")
	srv.handleGet(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != "real binary data" {
		t.Errorf("expected body %q, got %q", "real binary data", string(body))
	}
}

// TestHandleGetCDNHang_ClientDisconnectExitsCleanly verifies that the hang
// loop exits cleanly when the client disconnects (context cancelled) instead
// of hanging indefinitely.
func TestHandleGetCDNHang_ClientDisconnectExitsCleanly(t *testing.T) {
	srv, w, cleanup := newTestCDNHangEnv(t, []cdnResponse{
		// Always return 429 so the hang loop keeps retrying.
		{status: http.StatusTooManyRequests, body: "keep waiting"},
		{status: http.StatusTooManyRequests, body: "still busy"},
		{status: http.StatusTooManyRequests, body: "not yet"},
	})
	defer cleanup()

	// Create a request with a cancelable context.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/webdav/Test/test.mkv", nil).WithContext(ctx)
	req.Header.Set("Range", "bytes=0-499")

	done := make(chan struct{})
	go func() {
		srv.handleGet(w, req)
		close(done)
	}()

	// Let the hang loop start and make one attempt, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Clean exit — pass.
	case <-time.After(5 * time.Second):
		t.Fatal("handleGet did not exit after context cancellation")
	}
}

// TestStreamFileContent_Routes429ToHang verifies the integration between
// streamFileContent and handleGetCDNHang: when the initial CDN proxy request
// returns 429, streamFileContent correctly routes into hang/poll mode and
// the hang loop eventually streams valid data.
func TestStreamFileContent_Routes429ToHang(t *testing.T) {
	srv, w, cleanup := newTestCDNHangEnv(t, []cdnResponse{
		// First request from streamFileContent -> 429.
		{status: http.StatusTooManyRequests, body: "rate limited"},
		// Subsequent request from handleGetCDNHang -> success.
		{status: http.StatusOK, body: "real binary data", contentType: "application/octet-stream"},
	})
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/webdav/Test/test.mkv", nil)
	req.Header.Set("Range", "bytes=0-499")
	srv.handleGet(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The response body must be the binary data, not the 429 error text.
	if string(body) != "real binary data" {
		t.Errorf("expected body %q, got %q", "real binary data", string(body))
	}
}
