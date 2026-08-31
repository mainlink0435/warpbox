package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/mainlink0435/warpbox/internal/metadata"
	"github.com/mainlink0435/warpbox/internal/throttle"
)

// TestPlaybackNotBlockedBySync is the end-to-end regression test for the bug
// where the metadata sync's slow usenet pagination shared the single throttle
// queue and starved playback requestdl for 40-120s. With the split, a cold-play
// fetchCDNURL must complete while a sync request is still executing on its own
// (sync) queue.
func TestPlaybackNotBlockedBySync(t *testing.T) {
	srv, _, cleanup := newTestCDNHangEnv(t, []cdnResponse{
		{status: http.StatusOK, body: "data", contentType: "video/x-matroska"},
	})
	defer cleanup()

	// Separate sync queue wired to the server (as main.go does via SetSyncQueue).
	syncQueue := throttle.NewQueue(600)
	syncCtx, syncCancel := context.WithCancel(context.Background())
	defer syncCancel()
	syncQueue.Start(syncCtx)
	srv.SetSyncQueue(syncQueue)

	syncStarted := make(chan struct{})
	release := make(chan struct{})
	syncQueue.Enqueue(throttle.Request{
		Label: "metadata sync: ListUsenet",
		Execute: func(context.Context) error {
			close(syncStarted)
			<-release // hold the sync queue open, like a ~60s usenet page fetch
			return nil
		},
	})

	select {
	case <-syncStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("sync request never started")
	}

	type result struct {
		url string
		err error
	}
	done := make(chan result, 1)
	go func() {
		url, err := srv.fetchCDNURL(metadata.SourceTorrent, 1, 10)
		done <- result{url, err}
	}()

	// The cold-play requestdl must complete while the sync request is still
	// held open — proving a long-running sync call cannot starve playback.
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("fetchCDNURL failed while sync held open: %v", res.err)
		}
		if res.url == "" {
			t.Fatal("fetchCDNURL returned an empty URL")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cold-play requestdl blocked behind metadata sync")
	}

	close(release)
}

// TestThrottleStatsAggregation proves that the landing page's API-call counters
// (TotalCalls, SuccessfulCalls, etc.) are aggregated across both the playback
// queue and the metadata sync queue after SetSyncQueue.
func TestThrottleStatsAggregation(t *testing.T) {
	srv, _, cleanup := newTestCDNHangEnv(t, []cdnResponse{
		{status: http.StatusOK, body: "data", contentType: "video/x-matroska"},
	})
	defer cleanup()

	syncQueue := throttle.NewQueue(600)
	syncCtx, syncCancel := context.WithCancel(context.Background())
	defer syncCancel()
	syncQueue.Start(syncCtx)
	srv.SetSyncQueue(syncQueue)

	// One request on each queue.
	srv.queue.Enqueue(throttle.Request{Label: "playback", Execute: func(context.Context) error { return nil }})
	syncQueue.Enqueue(throttle.Request{Label: "sync", Execute: func(context.Context) error { return nil }})

	// Wait for both to complete.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ts := srv.throttleStats()
		if ts.TotalCalls >= 2 && ts.SuccessfulCalls >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	ts := srv.throttleStats()
	if ts.TotalCalls < 2 || ts.SuccessfulCalls < 2 {
		t.Fatalf("expected aggregated stats across both queues, got total=%d success=%d", ts.TotalCalls, ts.SuccessfulCalls)
	}

	// A 429 is recorded on BOTH queues by the global HTTP429Callback, so the
	// aggregated HTTP429Calls must count it once, not twice (regression guard
	// against double-counting in throttleStats).
	srv.queue.Record429()
	syncQueue.Record429()
	if n := srv.throttleStats().HTTP429Calls; n != 1 {
		t.Fatalf("expected 1 aggregated 429 (not double-counted), got %d", n)
	}
}
