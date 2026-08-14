package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mainlink0435/warpbox/internal/throttle"
	"github.com/mainlink0435/warpbox/internal/torbox"
)

func TestBuildFileRecordTorrent(t *testing.T) {
	f := torbox.TorrentFile{
		ID:        10,
		Name:      "movie.mkv",
		Size:      500,
		MimeType:  "video/x-matroska",
		S3Path:    "abc123/dir/movie.mkv",
		ShortName: "movie.mkv",
	}
	rec := buildFileRecord(42, f, 7, SourceTorrent, "2025-01-01T00:00:00Z")

	if rec.ItemID != 42 {
		t.Errorf("ItemID = %d, want 42", rec.ItemID)
	}
	if rec.FileID != 10 {
		t.Errorf("FileID = %d, want 10", rec.FileID)
	}
	if rec.Source != SourceTorrent {
		t.Errorf("Source = %d, want %d (SourceTorrent)", rec.Source, SourceTorrent)
	}
	if rec.SyncTag != 7 {
		t.Errorf("SyncTag = %d, want 7", rec.SyncTag)
	}
	if rec.CreatedAt != "2025-01-01T00:00:00Z" {
		t.Errorf("CreatedAt = %q, want %q", rec.CreatedAt, "2025-01-01T00:00:00Z")
	}
	if rec.Path != "dir/movie.mkv" {
		t.Errorf("Path = %q, want %q", rec.Path, "dir/movie.mkv")
	}
}

func TestBuildFileRecordUsenet(t *testing.T) {
	f := torbox.TorrentFile{
		ID:        20,
		Name:      "usenet_file.mkv",
		Size:      1000,
		MimeType:  "video/x-matroska",
		S3Path:    "def456/usenet_file.mkv",
		ShortName: "usenet_file.mkv",
	}
	rec := buildFileRecord(1644029, f, 3, SourceUsenet, "2025-06-01T12:00:00Z")

	if rec.ItemID != 1644029 {
		t.Errorf("ItemID = %d, want 1644029", rec.ItemID)
	}
	if rec.Source != SourceUsenet {
		t.Errorf("Source = %d, want %d (SourceUsenet)", rec.Source, SourceUsenet)
	}
	if rec.SyncTag != 3 {
		t.Errorf("SyncTag = %d, want 3", rec.SyncTag)
	}
	if rec.Path != "usenet_file.mkv" {
		t.Errorf("Path = %q, want %q", rec.Path, "usenet_file.mkv")
	}
}

func TestBuildFileRecordSingleFileAtRoot(t *testing.T) {
	// Single-file items have s3_path like "hash/filename.ext" with no directory.
	f := torbox.TorrentFile{
		ID:        1,
		S3Path:    "abc123/movie.mkv",
		ShortName: "movie.mkv",
	}
	rec := buildFileRecord(1, f, 1, SourceTorrent, "")

	if rec.Path != "movie.mkv" {
		t.Errorf("single-file Path = %q, want %q", rec.Path, "movie.mkv")
	}
}

func TestBuildFileRecordMultiFileWithDir(t *testing.T) {
	// Multi-file items have s3_path like "hash/dir/file.ext".
	f := torbox.TorrentFile{
		ID:        2,
		S3Path:    "abc123/Season 1/episode.mkv",
		ShortName: "episode.mkv",
	}
	rec := buildFileRecord(1, f, 1, SourceTorrent, "")

	if rec.Path != "Season 1/episode.mkv" {
		t.Errorf("multi-file Path = %q, want %q", rec.Path, "Season 1/episode.mkv")
	}
}

func TestBuildFileRecordSanitizesPath(t *testing.T) {
	// Characters like & should be replaced.
	f := torbox.TorrentFile{
		ID:        3,
		S3Path:    "abc123/A & B/show.mkv",
		ShortName: "show.mkv",
	}
	rec := buildFileRecord(1, f, 1, SourceTorrent, "")

	if rec.Path != "A _ B/show.mkv" {
		t.Errorf("sanitized Path = %q, want %q", rec.Path, "A _ B/show.mkv")
	}
	if rec.Name != "show.mkv" {
		t.Errorf("sanitized Name = %q, want %q", rec.Name, "show.mkv")
	}
}

func TestSyncWorker_Stop_BeforeStart(t *testing.T) {
	w := NewSyncWorker(nil, nil, nil, time.Minute, 5000, false, 3, time.Second, 0)
	w.Stop()
}

func TestSyncWorker_Restart_BeforeStart(t *testing.T) {
	w := NewSyncWorker(nil, nil, nil, time.Minute, 5000, false, 3, time.Second, 0)
	w.Restart()
}

func newTestSyncEnv(t *testing.T) (*SyncWorker, *httptest.Server, *Store, func()) {
	t.Helper()

	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[],"success":true}`))
	}))

	client := torbox.NewClient("test-api-key")
	client.SetBaseURL(ts.URL)
	client.SetHTTPClient(&http.Client{})

	queue := throttle.NewQueue(99999)
	qCtx, qCancel := context.WithCancel(context.Background())
	queue.Start(qCtx)

	sw := NewSyncWorker(store, client, queue, time.Hour, 5000, false, 3, time.Second, 0)

	cleanup := func() {
		qCancel()
		ts.Close()
		store.Close()
	}

	return sw, ts, store, cleanup
}

func TestSyncWorker_StartStop_Lifecycle(t *testing.T) {
	w, _, _, cleanup := newTestSyncEnv(t)
	defer cleanup()

	swCtx, swCancel := context.WithCancel(context.Background())
	defer swCancel()

	done := make(chan struct{})
	go func() {
		w.Start(swCtx)
		close(done)
	}()

	// Wait for the first sync cycle to complete.
	deadline := time.Now().Add(10 * time.Second)
	for w.Status().LastSuccess.IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for initial sync")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Stop the worker.
	stopDone := make(chan struct{})
	go func() {
		w.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not complete within 5s")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker goroutine did not exit after Stop")
	}
}

func newTestSyncEnvWithHandler(t *testing.T, handler http.HandlerFunc, retryAttempts int, retryBackoff time.Duration) (*SyncWorker, func()) {
	t.Helper()

	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(handler)

	client := torbox.NewClient("test-api-key")
	client.SetBaseURL(ts.URL)
	client.SetHTTPClient(&http.Client{})

	queue := throttle.NewQueue(99999)
	qCtx, qCancel := context.WithCancel(context.Background())
	queue.Start(qCtx)

	sw := NewSyncWorker(store, client, queue, time.Hour, 5000, false, retryAttempts, retryBackoff, 0)

	cleanup := func() {
		qCancel()
		ts.Close()
		store.Close()
	}

	return sw, cleanup
}

func TestSyncWorker_RetryOnTransientErrors(t *testing.T) {
	t.Run("retries succeed after transient failures", func(t *testing.T) {
		callCount := 0
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Fail on the first call (torrents attempt 0), succeed thereafter.
			if callCount < 1 {
				callCount++
				w.WriteHeader(http.StatusBadGateway)
				w.Write([]byte("error code: 502"))
				return
			}
			callCount++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"data":[],"success":true}`))
		})

		sw, cleanup := newTestSyncEnvWithHandler(t, handler, 1, 100*time.Millisecond)
		defer cleanup()

		sw.SyncNow()

		if sw.Status().LastError != "" {
			t.Fatalf("expected sync to succeed after retry, got error: %s", sw.Status().LastError)
		}
		if sw.Status().LastSuccess.IsZero() {
			t.Fatal("expected LastSuccess to be set after successful sync")
		}
	})

	t.Run("no retry means failure on first error", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("error code: 502"))
		})

		sw, cleanup := newTestSyncEnvWithHandler(t, handler, 0, time.Second)
		defer cleanup()

		sw.SyncNow()

		if sw.Status().LastError == "" {
			t.Fatal("expected sync to fail when retry_attempts is 0")
		}
	})
}

func TestSyncWorker_PruneKeepsRows_WhenUsenetFails(t *testing.T) {
	// Regression test for the data-loss bug: when the usenet fetch fails but
	// torrents succeed, the sync must NOT prune pre-existing usenet rows.
	// The previous gate (`torRes.err == nil || usenetRes.err == nil`) ran
	// PruneBySyncTag and deleted every usenet row on a single failed fetch.
	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}

	// Seed a pre-existing usenet row. SyncTag 0 marks it stale/unsynced, so it
	// would be deleted by any prune — under the old gate that happens the
	// moment usenet fails.
	seed := FileRecord{
		ItemID:  500,
		FileID:  700,
		Source:  SourceUsenet,
		Name:    "usenet_file.mkv",
		Path:    "usenet_file.mkv",
		Size:    100,
		SyncTag: 0,
	}
	if err := store.UpsertFile(seed); err != nil {
		t.Fatal(err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/usenet/mylist"):
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("error code: 502"))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"data":[],"success":true}`))
		}
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := torbox.NewClient("test-api-key")
	client.SetBaseURL(ts.URL)
	client.SetHTTPClient(&http.Client{})

	queue := throttle.NewQueue(99999)
	qCtx, qCancel := context.WithCancel(context.Background())
	defer qCancel()
	queue.Start(qCtx)

	sw := NewSyncWorker(store, client, queue, time.Hour, 5000, false, 0, time.Second, 0)
	sw.SyncNow()

	if sw.Status().LastError == "" {
		t.Fatal("expected sync to report an error because the usenet fetch failed")
	}

	got, err := store.GetFileByFileID(SourceUsenet, 700)
	if err != nil {
		t.Fatalf("querying seeded usenet row: %v", err)
	}
	if got == nil {
		t.Fatal("seeded usenet row was pruned after a failed usenet fetch; expected it to survive")
	}
}

func TestSyncWorker_InFlightGuard_SkipsConcurrentSync(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release // block so the first sync stays in flight
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[],"success":true}`))
	})

	sw, cleanup := newTestSyncEnvWithHandler(t, handler, 0, time.Second)
	defer cleanup()

	// First sync runs and blocks inside the handler.
	firstDone := make(chan struct{})
	go func() {
		sw.SyncNow()
		close(firstDone)
	}()
	<-started

	// While the first sync is in flight, Status() must report InProgress.
	if !sw.Status().InProgress {
		t.Error("expected InProgress=true while a sync is running")
	}

	// A second SyncNow must be skipped immediately by the in-flight guard.
	secondDone := make(chan struct{})
	go func() {
		sw.SyncNow()
		close(secondDone)
	}()
	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second SyncNow did not return promptly — guard did not skip it")
	}

	// Release the first sync so the test can finish cleanly.
	close(release)
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first sync did not finish after release")
	}

	// After the sync finishes, Status() must report not in progress.
	if sw.Status().InProgress {
		t.Error("expected InProgress=false after sync finished")
	}
}

func TestSyncWorker_SyncNow_HonorsConfiguredTimeout(t *testing.T) {
	// A handler that blocks. With a configured sync_timeout, SyncNow must abort
	// when the cap is hit instead of waiting forever (the old hardcoded 60s
	// cap is gone; the value is now configurable).
	block := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":[],"success":true}`))
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	store, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	client := torbox.NewClient("test-api-key")
	client.SetBaseURL(ts.URL)
	client.SetHTTPClient(&http.Client{})

	queue := throttle.NewQueue(99999)
	qCtx, qCancel := context.WithCancel(context.Background())
	defer qCancel()
	queue.Start(qCtx)

	// syncTimeout of 100ms: SyncNow should return (with an error) quickly.
	sw := NewSyncWorker(store, client, queue, time.Hour, 5000, false, 0, time.Second, 100*time.Millisecond)
	done := make(chan struct{})
	go func() {
		sw.SyncNow()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SyncNow did not return within the configured cap")
	}
	if sw.Status().LastError == "" {
		t.Error("expected LastError to be set when SyncNow hits its configured timeout")
	}

	close(block) // release the handler so the test exits cleanly
	time.Sleep(50 * time.Millisecond)
}

func TestSyncWorker_Restart_Lifecycle(t *testing.T) {
	w, _, _, cleanup := newTestSyncEnv(t)
	defer cleanup()

	swCtx, swCancel := context.WithCancel(context.Background())
	defer swCancel()

	done := make(chan struct{})
	go func() {
		w.Start(swCtx)
		close(done)
	}()

	// Wait for the first sync cycle to complete.
	deadline := time.Now().Add(10 * time.Second)
	for w.Status().LastSuccess.IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for initial sync")
		}
		time.Sleep(5 * time.Millisecond)
	}
	firstSync := w.Status().LastSuccess

	// Restart the worker.
	restartDone := make(chan struct{})
	go func() {
		w.Restart()
		close(restartDone)
	}()

	select {
	case <-restartDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Restart did not complete within 10s")
	}

	// Wait for the restarted loop to complete at least one sync cycle.
	syncDeadline := time.Now().Add(10 * time.Second)
	for {
		if w.Status().LastSuccess.After(firstSync) {
			break
		}
		if time.Now().After(syncDeadline) {
			t.Fatal("timed out waiting for restarted sync")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Stop the restarted worker.
	stopDone := make(chan struct{})
	go func() {
		w.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop after Restart did not complete within 5s")
	}
}