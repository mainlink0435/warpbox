package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// dbErr returns an item-scoped (DATABASE_ERROR) requestdl error.
func dbErr() error {
	return errors.New("torbox: unexpected status 500 (DATABASE_ERROR)")
}

func TestRecordTorrentFailure(t *testing.T) {
	srv := testServer(t, Config{
		CircuitBreakerFailures:  5,
		CircuitBreakerWindowSec: 60,
		CircuitBreakerStaleMin:  5,
	})

	itemID := int64(42)

	srv.recordTorrentFailure(itemID, failureKindDatabaseError, dbErr())
	tracker, exists := srv.torrentFailures[itemID]
	if !exists {
		t.Fatal("first failure: expected tracker to be created")
	}
	if len(tracker.failures) != 1 {
		t.Errorf("first failure: got %d failures, want 1", len(tracker.failures))
	}
	if !tracker.staleUntil.IsZero() {
		t.Errorf("first failure: staleUntil should be zero, got %v", tracker.staleUntil)
	}
}

func TestRecordTorrentFailure_belowThreshold(t *testing.T) {
	srv := testServer(t, Config{
		CircuitBreakerFailures:  5,
		CircuitBreakerWindowSec: 60,
		CircuitBreakerStaleMin:  5,
	})

	itemID := int64(42)
	for i := 0; i < 4; i++ {
		srv.recordTorrentFailure(itemID, failureKindDatabaseError, dbErr())
	}

	tracker := srv.torrentFailures[itemID]
	if len(tracker.failures) != 4 {
		t.Errorf("got %d failures, want 4", len(tracker.failures))
	}
	if !tracker.staleUntil.IsZero() {
		t.Errorf("below threshold: staleUntil should be zero, got %v", tracker.staleUntil)
	}
}

func TestRecordTorrentFailure_hitsThreshold(t *testing.T) {
	srv := testServer(t, Config{
		CircuitBreakerFailures:  5,
		CircuitBreakerWindowSec: 60,
		CircuitBreakerStaleMin:  5,
	})

	itemID := int64(42)
	for i := 0; i < 5; i++ {
		srv.recordTorrentFailure(itemID, failureKindDatabaseError, dbErr())
	}

	tracker := srv.torrentFailures[itemID]
	if len(tracker.failures) != 5 {
		t.Errorf("got %d failures, want 5", len(tracker.failures))
	}
	if tracker.staleUntil.IsZero() {
		t.Fatal("at threshold: staleUntil should be set")
	}
	expectedStale := time.Now().Add(5 * time.Minute)
	if tracker.staleUntil.Before(expectedStale.Add(-time.Second)) {
		t.Errorf("staleUntil too early: got %v, expected near %v", tracker.staleUntil, expectedStale)
	}
}

func TestRecordTorrentFailure_prunesOldFailures(t *testing.T) {
	srv := testServer(t, Config{
		CircuitBreakerFailures:  3,
		CircuitBreakerWindowSec: 60,
		CircuitBreakerStaleMin:  5,
	})

	itemID := int64(42)

	srv.torrentFailuresMu.Lock()
	oldTime := time.Now().Add(-120 * time.Second)
	tracker := &torrentFailureTracker{
		failures: []time.Time{oldTime, oldTime, oldTime},
	}
	srv.torrentFailures[itemID] = tracker
	srv.torrentFailuresMu.Unlock()

	srv.recordTorrentFailure(itemID, failureKindDatabaseError, dbErr())

	tracker = srv.torrentFailures[itemID]
	if len(tracker.failures) != 1 {
		t.Errorf("old failures should be pruned: got %d failures, want 1", len(tracker.failures))
	}
	if !tracker.staleUntil.IsZero() {
		t.Errorf("pruned case: staleUntil should be zero (only 1 active failure), got %v", tracker.staleUntil)
	}
}

func TestIsTorrentStale_noTracker(t *testing.T) {
	srv := testServer(t)
	if srv.isTorrentStale(42) {
		t.Error("no tracker: expected false")
	}
}

func TestIsTorrentStale_staleActive(t *testing.T) {
	srv := testServer(t)

	srv.torrentFailuresMu.Lock()
	srv.torrentFailures[42] = &torrentFailureTracker{
		staleUntil: time.Now().Add(5 * time.Minute),
	}
	srv.torrentFailuresMu.Unlock()

	if !srv.isTorrentStale(42) {
		t.Error("active stale period: expected true")
	}

	srv.torrentFailuresMu.Lock()
	if _, exists := srv.torrentFailures[42]; !exists {
		t.Error("active stale: tracker should still exist in map")
	}
	srv.torrentFailuresMu.Unlock()
}

func TestIsTorrentStale_periodExpiredProbes(t *testing.T) {
	srv := testServer(t)

	srv.torrentFailuresMu.Lock()
	srv.torrentFailures[42] = &torrentFailureTracker{
		staleUntil: time.Now().Add(-1 * time.Minute),
	}
	srv.torrentFailuresMu.Unlock()

	if srv.isTorrentStale(42) {
		t.Error("expired stale period: expected false (half-open probe allowed)")
	}

	srv.torrentFailuresMu.Lock()
	tracker, exists := srv.torrentFailures[42]
	srv.torrentFailuresMu.Unlock()
	if !exists {
		t.Error("expired stale: tracker should remain (probe lock re-armed)")
	}
	if !tracker.staleUntil.After(time.Now()) {
		t.Error("expired stale: probe lock should be re-armed into the future")
	}
}

func TestIsTorrentStale_notYetStale(t *testing.T) {
	srv := testServer(t)

	srv.torrentFailuresMu.Lock()
	srv.torrentFailures[42] = &torrentFailureTracker{
		failures: []time.Time{time.Now()},
	}
	srv.torrentFailuresMu.Unlock()

	if srv.isTorrentStale(42) {
		t.Error("failures but not stale: expected false")
	}

	srv.torrentFailuresMu.Lock()
	if _, exists := srv.torrentFailures[42]; !exists {
		t.Error("not stale: tracker should still exist")
	}
	srv.torrentFailuresMu.Unlock()
}

func TestSweepNegativeCache_removesExpired(t *testing.T) {
	srv := testServer(t, Config{NegativeCacheMaxEntries: 10})

	srv.negativeCacheMu.Lock()
	srv.negativeCache["expired1"] = &negativeCacheEntry{expiresAt: time.Now().Add(-1 * time.Minute)}
	srv.negativeCache["expired2"] = &negativeCacheEntry{expiresAt: time.Now().Add(-2 * time.Minute)}
	srv.negativeCache["fresh"] = &negativeCacheEntry{expiresAt: time.Now().Add(1 * time.Minute)}
	srv.negativeCacheMu.Unlock()

	srv.sweepNegativeCache()

	srv.negativeCacheMu.Lock()
	if _, exists := srv.negativeCache["expired1"]; exists {
		t.Error("expired entry 1 should have been removed")
	}
	if _, exists := srv.negativeCache["expired2"]; exists {
		t.Error("expired entry 2 should have been removed")
	}
	if _, exists := srv.negativeCache["fresh"]; !exists {
		t.Error("fresh entry should still exist")
	}
	if len(srv.negativeCache) != 1 {
		t.Errorf("expected 1 entry, got %d", len(srv.negativeCache))
	}
	srv.negativeCacheMu.Unlock()
}

func TestSweepNegativeCache_allExpired(t *testing.T) {
	srv := testServer(t, Config{NegativeCacheMaxEntries: 10})

	srv.negativeCacheMu.Lock()
	srv.negativeCache["a"] = &negativeCacheEntry{expiresAt: time.Now().Add(-1 * time.Minute)}
	srv.negativeCache["b"] = &negativeCacheEntry{expiresAt: time.Now().Add(-2 * time.Minute)}
	srv.negativeCacheMu.Unlock()

	srv.sweepNegativeCache()

	srv.negativeCacheMu.Lock()
	if len(srv.negativeCache) != 0 {
		t.Errorf("all expired: expected 0 entries, got %d", len(srv.negativeCache))
	}
	srv.negativeCacheMu.Unlock()
}

func TestSweepNegativeCache_noneExpired(t *testing.T) {
	srv := testServer(t, Config{NegativeCacheMaxEntries: 10})

	srv.negativeCacheMu.Lock()
	srv.negativeCache["a"] = &negativeCacheEntry{expiresAt: time.Now().Add(1 * time.Minute)}
	srv.negativeCache["b"] = &negativeCacheEntry{expiresAt: time.Now().Add(2 * time.Minute)}
	srv.negativeCacheMu.Unlock()

	srv.sweepNegativeCache()

	srv.negativeCacheMu.Lock()
	if len(srv.negativeCache) != 2 {
		t.Errorf("none expired: expected 2 entries, got %d", len(srv.negativeCache))
	}
	srv.negativeCacheMu.Unlock()
}

func TestSweepNegativeCache_empty(t *testing.T) {
	srv := testServer(t, Config{NegativeCacheMaxEntries: 10})

	srv.sweepNegativeCache()

	srv.negativeCacheMu.Lock()
	if len(srv.negativeCache) != 0 {
		t.Errorf("empty: expected 0 entries, got %d", len(srv.negativeCache))
	}
	srv.negativeCacheMu.Unlock()
}

func TestSweepNegativeCache_evictsOldestWhenOverMax(t *testing.T) {
	srv := testServer(t, Config{NegativeCacheMaxEntries: 3})

	now := time.Now()
	srv.negativeCacheMu.Lock()
	srv.negativeCache["a"] = &negativeCacheEntry{expiresAt: now.Add(1 * time.Minute)}
	srv.negativeCache["b"] = &negativeCacheEntry{expiresAt: now.Add(2 * time.Minute)}
	srv.negativeCache["c"] = &negativeCacheEntry{expiresAt: now.Add(3 * time.Minute)}
	srv.negativeCache["d"] = &negativeCacheEntry{expiresAt: now.Add(4 * time.Minute)}
	srv.negativeCacheMu.Unlock()

	srv.sweepNegativeCache()

	srv.negativeCacheMu.Lock()
	if len(srv.negativeCache) != 3 {
		t.Errorf("over max: expected 3 entries, got %d", len(srv.negativeCache))
	}
	// a (oldest expiring) should have been evicted
	if _, exists := srv.negativeCache["a"]; exists {
		t.Error("oldest entry 'a' should have been evicted")
	}
	for _, k := range []string{"b", "c", "d"} {
		if _, exists := srv.negativeCache[k]; !exists {
			t.Errorf("entry %q should have survived", k)
		}
	}
	srv.negativeCacheMu.Unlock()
}

func TestSweepNegativeCache_evictsAfterExpiredRemoved(t *testing.T) {
	srv := testServer(t, Config{NegativeCacheMaxEntries: 2})

	now := time.Now()
	srv.negativeCacheMu.Lock()
	srv.negativeCache["exp"] = &negativeCacheEntry{expiresAt: now.Add(-1 * time.Minute)}
	srv.negativeCache["a"] = &negativeCacheEntry{expiresAt: now.Add(1 * time.Minute)}
	srv.negativeCache["b"] = &negativeCacheEntry{expiresAt: now.Add(2 * time.Minute)}
	srv.negativeCache["c"] = &negativeCacheEntry{expiresAt: now.Add(3 * time.Minute)}
	srv.negativeCacheMu.Unlock()

	srv.sweepNegativeCache()

	srv.negativeCacheMu.Lock()
	if len(srv.negativeCache) != 2 {
		t.Errorf("expected 2 entries after sweep+evict, got %d", len(srv.negativeCache))
	}
	if _, exists := srv.negativeCache["exp"]; exists {
		t.Error("expired entry should have been removed")
	}
	if _, exists := srv.negativeCache["a"]; exists {
		t.Error("oldest surviving entry 'a' should have been evicted")
	}
	srv.negativeCacheMu.Unlock()
}

func TestSweepCircuitBreaker_removesExpired(t *testing.T) {
	srv := testServer(t, Config{CircuitBreakerMaxEntries: 10})

	srv.torrentFailuresMu.Lock()
	srv.torrentFailures[1] = &torrentFailureTracker{staleUntil: time.Now().Add(-1 * time.Minute)}
	srv.torrentFailures[2] = &torrentFailureTracker{staleUntil: time.Now().Add(1 * time.Minute)}
	srv.torrentFailures[3] = &torrentFailureTracker{staleUntil: time.Now().Add(-2 * time.Minute)}
	srv.torrentFailuresMu.Unlock()

	srv.sweepCircuitBreaker()

	srv.torrentFailuresMu.Lock()
	if _, exists := srv.torrentFailures[1]; exists {
		t.Error("expired tracker 1 should have been removed")
	}
	if _, exists := srv.torrentFailures[3]; exists {
		t.Error("expired tracker 3 should have been removed")
	}
	if _, exists := srv.torrentFailures[2]; !exists {
		t.Error("active tracker 2 should survive")
	}
	if len(srv.torrentFailures) != 1 {
		t.Errorf("expected 1 tracker, got %d", len(srv.torrentFailures))
	}
	srv.torrentFailuresMu.Unlock()
}

func TestSweepCircuitBreaker_neverStaleSurvives(t *testing.T) {
	srv := testServer(t, Config{CircuitBreakerMaxEntries: 10})

	srv.torrentFailuresMu.Lock()
	srv.torrentFailures[1] = &torrentFailureTracker{
		failures:   []time.Time{time.Now()},
		staleUntil: time.Time{}, // zero — never went stale
	}
	srv.torrentFailures[2] = &torrentFailureTracker{staleUntil: time.Now().Add(-1 * time.Minute)}
	srv.torrentFailuresMu.Unlock()

	srv.sweepCircuitBreaker()

	srv.torrentFailuresMu.Lock()
	if _, exists := srv.torrentFailures[1]; !exists {
		t.Error("never-stale tracker 1 should survive")
	}
	if _, exists := srv.torrentFailures[2]; exists {
		t.Error("expired tracker 2 should have been removed")
	}
	srv.torrentFailuresMu.Unlock()
}

func TestSweepCircuitBreaker_empty(t *testing.T) {
	srv := testServer(t, Config{CircuitBreakerMaxEntries: 10})

	srv.sweepCircuitBreaker()

	srv.torrentFailuresMu.Lock()
	if len(srv.torrentFailures) != 0 {
		t.Errorf("empty: expected 0, got %d", len(srv.torrentFailures))
	}
	srv.torrentFailuresMu.Unlock()
}

func TestSweepCircuitBreaker_evictsOldestWhenOverMax(t *testing.T) {
	srv := testServer(t, Config{CircuitBreakerMaxEntries: 2})

	now := time.Now()
	srv.torrentFailuresMu.Lock()
	srv.torrentFailures[1] = &torrentFailureTracker{staleUntil: now.Add(1 * time.Minute)}
	srv.torrentFailures[2] = &torrentFailureTracker{staleUntil: now.Add(2 * time.Minute)}
	srv.torrentFailures[3] = &torrentFailureTracker{staleUntil: now.Add(3 * time.Minute)}
	srv.torrentFailuresMu.Unlock()

	srv.sweepCircuitBreaker()

	srv.torrentFailuresMu.Lock()
	if len(srv.torrentFailures) != 2 {
		t.Errorf("over max: expected 2 entries, got %d", len(srv.torrentFailures))
	}
	// 1 has the oldest (soonest) staleUntil — should be evicted
	if _, exists := srv.torrentFailures[1]; exists {
		t.Error("oldest entry 1 should have been evicted")
	}
	for _, id := range []int64{2, 3} {
		if _, exists := srv.torrentFailures[id]; !exists {
			t.Errorf("entry %d should have survived", id)
		}
	}
	srv.torrentFailuresMu.Unlock()
}

func TestHandleStatsJSON_returnsMetrics(t *testing.T) {
	srv := testServer(t, Config{
		Version:           "test",
		StatsChartMinutes: 60,
	})

	if err := srv.store.RecordStats(map[string]float64{
		"api_calls_success": 42,
		"api_calls_failed":  3,
	}); err != nil {
		t.Fatalf("RecordStats failed: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/stats.json", nil)
	r.Header.Set("X-CSRF-Token", srv.csrfToken)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}

	var data map[string][]map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	if len(data) != 2 {
		t.Errorf("expected 2 metric keys, got %d", len(data))
	}

	success, ok := data["api_calls_success"]
	if !ok {
		t.Fatal("expected key 'api_calls_success'")
	}
	if len(success) != 1 {
		t.Fatalf("expected 1 data point for success, got %d", len(success))
	}
	if v, ok := success[0]["v"].(float64); !ok || v != 42 {
		t.Errorf("expected value 42, got %v", success[0]["v"])
	}
	if tStr, ok := success[0]["t"].(string); !ok || tStr == "" {
		t.Errorf("expected non-empty timestamp, got %q", tStr)
	}

	failed, ok := data["api_calls_failed"]
	if !ok {
		t.Fatal("expected key 'api_calls_failed'")
	}
	if len(failed) != 1 {
		t.Fatalf("expected 1 data point for failed, got %d", len(failed))
	}
	if v, ok := failed[0]["v"].(float64); !ok || v != 3 {
		t.Errorf("expected value 3, got %v", failed[0]["v"])
	}
}

func TestHandleStatsJSON_emptyStore(t *testing.T) {
	srv := testServer(t, Config{
		Version:           "test",
		StatsChartMinutes: 60,
	})

	r := httptest.NewRequest(http.MethodGet, "/stats.json", nil)
	r.Header.Set("X-CSRF-Token", srv.csrfToken)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("expected empty JSON object, got %d keys", len(data))
	}
}

func TestHandleStatsJSON_unauthenticated(t *testing.T) {
	srv := testServer(t, Config{
		Version:      "test",
		AuthEnabled:  true,
		AuthUsername: "admin",
		AuthPassword: "secret",
	})

	r := httptest.NewRequest(http.MethodGet, "/stats.json", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 without auth, got %d", resp.StatusCode)
	}
}

func TestRecordStats_storesAllMetrics(t *testing.T) {
	srv := testServer(t, Config{
		Version: "test",
	})

	// Seed requestdl outcomes so the api-health gauge is recorded.
	srv.recordOutcome(true)
	srv.recordOutcome(true)
	srv.recordOutcome(false)

	srv.recordStats()

	since := time.Now().Add(-24 * time.Hour)
	metrics, err := srv.store.QueryAllStatsSince(since)
	if err != nil {
		t.Fatalf("QueryAllStatsSince failed: %v", err)
	}

	expectedMetrics := []string{
		"api_calls_success",
		"api_calls_failed",
		"api_calls_429",
		"db_lock_errors",
		"gc_cycles",
		"sys_mb",
		"alloc_mb",
		"heap_objects",
		"negative_cache_entries",
		"circuit_breaker_entries",
		"requestdl_success_ratio",
	}

	for _, name := range expectedMetrics {
		if _, ok := metrics[name]; !ok {
			t.Errorf("expected metric %q not found in stored data", name)
		}
	}

	if len(metrics) != len(expectedMetrics) {
		t.Errorf("expected %d metrics, got %d", len(expectedMetrics), len(metrics))
	}
}

func TestRecordStats_zeroDeltasOnSecondCall(t *testing.T) {
	srv := testServer(t, Config{
		Version: "test",
	})

	srv.recordStats()
	srv.recordStats()

	since := time.Now().Add(-24 * time.Hour)
	metrics, err := srv.store.QueryAllStatsSince(since)
	if err != nil {
		t.Fatalf("QueryAllStatsSince failed: %v", err)
	}

	successRecords := metrics["api_calls_success"]
	if len(successRecords) != 2 {
		t.Fatalf("expected 2 records for api_calls_success, got %d", len(successRecords))
	}

	if successRecords[1].Value != 0 {
		t.Errorf("second call: expected delta 0 for api_calls_success, got %f", successRecords[1].Value)
	}

	sysRecords := metrics["sys_mb"]
	if len(sysRecords) != 2 {
		t.Fatalf("expected 2 records for sys_mb, got %d", len(sysRecords))
	}
	if sysRecords[1].Value <= 0 {
		t.Errorf("sys_mb should be positive, got %f", sysRecords[1].Value)
	}
}

func TestHandleStatsJSON_minutesFallback(t *testing.T) {
	srv := testServer(t, Config{
		Version:           "test",
		StatsChartMinutes: 0,
	})

	if err := srv.store.RecordStats(map[string]float64{"test": 1.0}); err != nil {
		t.Fatalf("RecordStats failed: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/stats.json", nil)
	r.Header.Set("X-CSRF-Token", srv.csrfToken)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestClassifyTorboxError(t *testing.T) {
	cases := []struct {
		err  error
		kind failureKind
		code string
	}{
		{errors.New("torbox: unexpected status 500 (DATABASE_ERROR)"), failureKindDatabaseError, "DATABASE_ERROR"},
		{errors.New("torbox: unexpected status 401 (BAD_TOKEN)"), failureKindAuth, "BAD_TOKEN"},
		{errors.New("torbox: unexpected status 429"), failureKindTransient, ""},
		{errors.New("torbox: unexpected status 500"), failureKindTransient, ""},
		{errors.New("torbox: unexpected status 429 (RATE_LIMIT)"), failureKindTransient, "RATE_LIMIT"},
		{errors.New("torbox: connection refused"), failureKindTransient, ""},
		{nil, failureKindTransient, ""},
	}
	for _, c := range cases {
		kind, code := classifyTorboxError(c.err)
		if kind != c.kind || code != c.code {
			t.Errorf("classify(%v) = (%q, %q), want (%q, %q)", c.err, kind, code, c.kind, c.code)
		}
	}
}

func TestRecordTorrentFailure_escalatesStaleWindow(t *testing.T) {
	srv := testServer(t, Config{
		CircuitBreakerFailures:    3,
		CircuitBreakerWindowSec:   60,
		CircuitBreakerStaleMin:    1,
		CircuitBreakerMaxStaleMin: 4,
	})
	itemID := int64(7)
	for i := 0; i < 3; i++ {
		srv.recordTorrentFailure(itemID, failureKindDatabaseError, dbErr())
	}
	srv.torrentFailuresMu.Lock()
	t1 := srv.torrentFailures[itemID]
	d1 := time.Until(t1.staleUntil)
	esc1 := t1.escalations
	srv.torrentFailuresMu.Unlock()
	if d1 < 50*time.Second || d1 > 70*time.Second {
		t.Errorf("first trip stale window = %v, want ~1m", d1)
	}
	if esc1 != 1 {
		t.Errorf("escalations after first trip = %d, want 1", esc1)
	}
	// More failures escalate toward the cap (4m).
	for i := 0; i < 6; i++ {
		srv.recordTorrentFailure(itemID, failureKindDatabaseError, dbErr())
	}
	srv.torrentFailuresMu.Lock()
	t2 := srv.torrentFailures[itemID]
	d2 := time.Until(t2.staleUntil)
	esc2 := t2.escalations
	srv.torrentFailuresMu.Unlock()
	if d2 < 3*time.Minute || d2 > 5*time.Minute {
		t.Errorf("escalated stale window = %v, want capped ~4m", d2)
	}
	if esc2 <= esc1 {
		t.Errorf("escalations should increase: got %d (was %d)", esc2, esc1)
	}
}

func TestRecordTorrentFailure_transientDoesNotEscalate(t *testing.T) {
	srv := testServer(t, Config{
		CircuitBreakerFailures:    3,
		CircuitBreakerWindowSec:   60,
		CircuitBreakerStaleMin:    1,
		CircuitBreakerMaxStaleMin: 4,
	})
	itemID := int64(8)
	for i := 0; i < 3; i++ {
		srv.recordTorrentFailure(itemID, failureKindTransient, errors.New("torbox: unexpected status 429"))
	}
	srv.torrentFailuresMu.Lock()
	tracker := srv.torrentFailures[itemID]
	esc := tracker.escalations
	notified := tracker.quarantineNotified
	srv.torrentFailuresMu.Unlock()
	if esc != 0 {
		t.Errorf("transient: escalations should be 0, got %d", esc)
	}
	if notified {
		t.Error("transient: should not emit quarantine-notified")
	}
}

func TestRecordTorrentFailure_globalFlapDoesNotEscalate(t *testing.T) {
	srv := testServer(t, Config{
		CircuitBreakerFailures:    3,
		CircuitBreakerWindowSec:   60,
		CircuitBreakerStaleMin:    1,
		CircuitBreakerMaxStaleMin: 4,
	})
	// Simulate a TorBox-wide outage: only failures in the health window.
	for i := 0; i < globalHealthMinSamples; i++ {
		srv.recordOutcome(false)
	}
	if !srv.globalDegraded() {
		t.Fatal("expected globalDegraded() true after all-failure window")
	}
	itemID := int64(9)
	for i := 0; i < 3; i++ {
		srv.recordTorrentFailure(itemID, failureKindDatabaseError, dbErr())
	}
	srv.torrentFailuresMu.Lock()
	tracker := srv.torrentFailures[itemID]
	esc := tracker.escalations
	notified := tracker.quarantineNotified
	kind := tracker.errKind
	d := time.Until(tracker.staleUntil)
	srv.torrentFailuresMu.Unlock()
	if kind != failureKindTransient {
		t.Errorf("global flap: errKind should be downgraded to transient, got %q", kind)
	}
	if esc != 0 {
		t.Errorf("global flap: escalations should be 0, got %d", esc)
	}
	if notified {
		t.Error("global flap: should not emit quarantine-notified")
	}
	if d < 50*time.Second || d > 70*time.Second {
		t.Errorf("global flap: stale window = %v, want fixed base ~1m", d)
	}
}

func TestQuarantinedItems(t *testing.T) {
	srv := testServer(t)
	now := time.Now()
	srv.torrentFailuresMu.Lock()
	srv.torrentFailures[42] = &torrentFailureTracker{
		failures:    []time.Time{now.Add(-10 * time.Second), now.Add(-5 * time.Second), now},
		staleUntil:  now.Add(5 * time.Minute),
		errKind:     failureKindDatabaseError,
		errorCode:   "DATABASE_ERROR",
		lastErr:     dbErr().Error(),
		trippedAt:   now,
		escalations: 2,
	}
	srv.torrentFailuresMu.Unlock()

	items := srv.QuarantinedItems()
	if len(items) != 1 {
		t.Fatalf("expected 1 quarantined item, got %d", len(items))
	}
	it := items[0]
	if it.ItemID != 42 {
		t.Errorf("ItemID = %d, want 42", it.ItemID)
	}
	if it.Label != "item #42" {
		t.Errorf("Label = %q, want fallback item #42", it.Label)
	}
	if it.ErrKind != failureKindDatabaseError {
		t.Errorf("ErrKind = %q, want database_error", it.ErrKind)
	}
	if it.ErrorCode != "DATABASE_ERROR" {
		t.Errorf("ErrorCode = %q, want DATABASE_ERROR", it.ErrorCode)
	}
	if it.Failures != 3 {
		t.Errorf("Failures = %d, want 3", it.Failures)
	}
	if it.Escalations != 2 {
		t.Errorf("Escalations = %d, want 2", it.Escalations)
	}
}

func TestPruneBreakerForMissingItems(t *testing.T) {
	srv := testServer(t)
	srv.torrentFailuresMu.Lock()
	srv.torrentFailures[1] = &torrentFailureTracker{staleUntil: time.Now().Add(time.Minute)}
	srv.torrentFailures[2] = &torrentFailureTracker{staleUntil: time.Now().Add(time.Minute)}
	srv.torrentFailures[3] = &torrentFailureTracker{staleUntil: time.Now().Add(time.Minute)}
	srv.torrentFailuresMu.Unlock()

	srv.negativeCacheMu.Lock()
	srv.negativeCache["torrent:3:5"] = &negativeCacheEntry{err: errors.New("x"), expiresAt: time.Now().Add(time.Minute)}
	srv.negativeCache["torrent:1:9"] = &negativeCacheEntry{err: errors.New("y"), expiresAt: time.Now().Add(time.Minute)}
	srv.negativeCacheMu.Unlock()

	srv.PruneBreakerForMissingItems(map[int64]struct{}{1: {}, 2: {}})

	srv.torrentFailuresMu.Lock()
	_, ok1 := srv.torrentFailures[1]
	_, ok2 := srv.torrentFailures[2]
	_, ok3 := srv.torrentFailures[3]
	srv.torrentFailuresMu.Unlock()
	if !ok1 || !ok2 {
		t.Error("live items should remain in the breaker")
	}
	if ok3 {
		t.Error("missing item should be pruned from the breaker")
	}

	srv.negativeCacheMu.Lock()
	_, n1 := srv.negativeCache["torrent:3:5"]
	_, n2 := srv.negativeCache["torrent:1:9"]
	srv.negativeCacheMu.Unlock()
	if n1 {
		t.Error("missing item's negative-cache entry should be pruned")
	}
	if !n2 {
		t.Error("live item's negative-cache entry should remain")
	}
}

func TestRecordTorrentFailure_persistentLowRateTrips(t *testing.T) {
	srv := testServer(t, Config{
		CircuitBreakerFailures:     5,
		CircuitBreakerWindowSec:    600,
		CircuitBreakerStaleMin:     1,
		CircuitBreakerMaxStaleMin:  4,
	})
	now := time.Now()
	// 5 failures spread over ~8 minutes — previously pruned by a 60s window,
	// now within the 600s window, so the next failure trips the breaker.
	srv.torrentFailuresMu.Lock()
	srv.torrentFailures[42] = &torrentFailureTracker{
		failures: []time.Time{
			now.Add(-8 * time.Minute),
			now.Add(-6 * time.Minute),
			now.Add(-4 * time.Minute),
			now.Add(-2 * time.Minute),
			now.Add(-1 * time.Minute),
		},
	}
	srv.torrentFailuresMu.Unlock()

	srv.recordTorrentFailure(42, failureKindDatabaseError, dbErr())

	srv.torrentFailuresMu.Lock()
	tracker := srv.torrentFailures[42]
	tripped := !tracker.staleUntil.IsZero()
	esc := tracker.escalations
	srv.torrentFailuresMu.Unlock()
	if !tripped {
		t.Error("expected the breaker to trip after persistent low-rate failures")
	}
	if esc != 1 {
		t.Errorf("escalations = %d, want 1", esc)
	}
}

func TestRequestdlHealth(t *testing.T) {
	srv := testServer(t)
	if h := srv.requestdlHealth(); !h.TooFew {
		t.Error("expected TooFew with no samples")
	}

	srv.recordOutcome(true)
	srv.recordOutcome(true)
	srv.recordOutcome(true)
	srv.recordOutcome(true)
	srv.recordOutcome(false)
	srv.recordOutcome(false)
	h := srv.requestdlHealth()
	if h.TooFew {
		t.Error("expected not TooFew with 6 samples")
	}
	if h.Samples != 6 || h.Successes != 4 || h.Failures != 2 {
		t.Errorf("got samples=%d succ=%d fails=%d, want 6/4/2", h.Samples, h.Successes, h.Failures)
	}
	if h.Ratio != 4.0/6.0 {
		t.Errorf("ratio = %v, want 0.666", h.Ratio)
	}
	if h.Degraded {
		t.Error("expected not degraded at 4/6 success")
	}

	// All failures → degraded.
	srv = testServer(t)
	for i := 0; i < globalHealthMinSamples; i++ {
		srv.recordOutcome(false)
	}
	h = srv.requestdlHealth()
	if h.TooFew {
		t.Error("expected not TooFew with enough all-failure samples")
	}
	if !h.Degraded {
		t.Error("expected degraded with all failures")
	}
}

// TestRecordTorBoxOutcome_countsRetriedRecoveredFailures verifies the public
// health hook counts raw HTTP attempts — a call that fails twice and then
// succeeds on a retry still registers 2 failures, so flapping provider
// degradation no longer reads as "Failures: 0".
func TestRecordTorBoxOutcome_countsRetriedRecoveredFailures(t *testing.T) {
	srv := testServer(t)

	// Simulate retry-and-recover: two non-200 responses then a 200.
	srv.RecordTorBoxOutcome("/v1/api/torrents/mylist", 500, false)
	srv.RecordTorBoxOutcome("/v1/api/torrents/mylist", 502, false)
	srv.RecordTorBoxOutcome("/v1/api/torrents/mylist", 200, true)

	h := srv.requestdlHealth()
	if h.Samples != 3 {
		t.Errorf("samples = %d, want 3", h.Samples)
	}
	if h.Failures != 2 {
		t.Errorf("failures = %d, want 2 (retried-and-recovered failures must count)", h.Failures)
	}
	if h.Successes != 1 {
		t.Errorf("successes = %d, want 1", h.Successes)
	}
}

func TestQuarantinedItems_listsTrippedAndFailing(t *testing.T) {
	srv := testServer(t)
	now := time.Now()
	srv.torrentFailuresMu.Lock()
	// Tripped item — quarantined.
	srv.torrentFailures[1] = &torrentFailureTracker{
		failures:   []time.Time{now},
		staleUntil: now.Add(5 * time.Minute),
		errKind:    failureKindDatabaseError,
		errorCode:  "DATABASE_ERROR",
		trippedAt:  now,
	}
	// Untripped (still accumulating) item — failing but not quarantined.
	srv.torrentFailures[2] = &torrentFailureTracker{
		failures: []time.Time{now},
		errKind:  failureKindDatabaseError,
	}
	srv.torrentFailuresMu.Unlock()

	items := srv.QuarantinedItems()
	if len(items) != 2 {
		t.Fatalf("expected 2 items (tripped + failing), got %d", len(items))
	}
	for _, it := range items {
		switch it.ItemID {
		case 1:
			if !it.Tripped {
				t.Error("item 1 should be marked Tripped")
			}
		case 2:
			if it.Tripped {
				t.Error("item 2 should not be marked Tripped")
			}
		default:
			t.Errorf("unexpected item id %d", it.ItemID)
		}
	}
}

func TestAPIHealthTimeDisplays(t *testing.T) {
	var h APIHealth
	if got := h.LastSuccessDisplay(); got != "—" {
		t.Errorf("zero LastSuccessDisplay = %q, want —", got)
	}
	if got := h.LastFailureDisplay(); got != "—" {
		t.Errorf("zero LastFailureDisplay = %q, want —", got)
	}

	when := time.Now().Add(-3 * time.Minute)
	h = APIHealth{LastSuccess: when, LastFailure: when}
	for name, got := range map[string]string{
		"LastSuccessDisplay": h.LastSuccessDisplay(),
		"LastFailureDisplay": h.LastFailureDisplay(),
	} {
		if !strings.Contains(got, "ago") {
			t.Errorf("%s = %q, want relative 'ago' context", name, got)
		}
		if !strings.Contains(got, when.Format("Jan 2 15:04")) {
			t.Errorf("%s = %q, want date present", name, got)
		}
	}
}
