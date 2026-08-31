package throttle

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnqueueDequeue(t *testing.T) {
	q := NewQueue(100) // 100 req/min = 600ms spacing
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	var count int32
	for i := 0; i < 3; i++ {
		q.Enqueue(Request{
			Label: "test",
			Execute: func(ctx context.Context) error {
				atomic.AddInt32(&count, 1)
				return nil
			},
		})
	}

	// Wait for all three to process (~1.8s with 600ms spacing).
	time.Sleep(2 * time.Second)
	if n := atomic.LoadInt32(&count); n != 3 {
		t.Errorf("expected 3 requests processed, got %d", n)
	}
}

func TestEnqueueOrder(t *testing.T) {
	q := NewQueue(600) // 600 req/min = 100ms spacing
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	var results []int
	order := make(chan int, 3)

	for i := 0; i < 3; i++ {
		i := i
		q.Enqueue(Request{
			Label: "order",
			Execute: func(ctx context.Context) error {
				order <- i
				return nil
			},
		})
	}

	time.Sleep(500 * time.Millisecond)
	close(order)
	for v := range order {
		results = append(results, v)
	}

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for i, v := range results {
		if v != i {
			t.Errorf("expected order[%d]=%d, got %d", i, i, v)
		}
	}
}

func TestContextCancellation(t *testing.T) {
	q := NewQueue(100)
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)

	var executed bool
	q.Enqueue(Request{
		Label: "cancel-test",
		Execute: func(ctx context.Context) error {
			executed = true
			return nil
		},
	})

	// Cancel before processing completes.
	time.Sleep(10 * time.Millisecond)
	cancel()

	// Give it a moment.
	time.Sleep(500 * time.Millisecond)
	if executed {
		t.Log("request may have executed before cancellation (race allowed)")
	}
}

func TestConcurrentStress50At280RPM(t *testing.T) {
	// 50 concurrent enqueues at 280 req/min (≈214ms spacing).
	// Total expected time: 50 * 214ms ≈ 10.7s + some overhead.
	q := NewQueue(280)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	q.Start(ctx)

	var executed int32
	done := make(chan struct{})

	// Launch 50 goroutines all at once.
	for i := 0; i < 50; i++ {
		go func(n int) {
			q.Enqueue(Request{
				Label: "stress",
				Execute: func(ctx context.Context) error {
					atomic.AddInt32(&executed, 1)
					return nil
				},
			})
		}(i)
	}

	// Wait for all 50 to complete.
	start := time.Now()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-ticker.C:
			if n := atomic.LoadInt32(&executed); n >= 50 {
				close(done)
				break loop
			}
		case <-ctx.Done():
			t.Fatalf("timeout waiting for 50 requests to execute; executed=%d", atomic.LoadInt32(&executed))
		}
	}

	<-done
	elapsed := time.Since(start)
	n := atomic.LoadInt32(&executed)

	if n != 50 {
		t.Errorf("expected 50 executed, got %d", n)
	}
	t.Logf("50 concurrent requests at 280 req/min: completed in %v", elapsed.Round(time.Millisecond))
}

func TestErrorDoesNotDeadlock(t *testing.T) {
	q := NewQueue(600) // 100ms spacing
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q.Start(ctx)

	var goodCount int32

	// Enqueue a mix of erroring and successful requests.
	for i := 0; i < 5; i++ {
		i := i
		q.Enqueue(Request{
			Label: "error-test",
			Execute: func(ctx context.Context) error {
				if i%2 == 0 {
					return nil // success
				}
				return &mockError{msg: "simulated 429"}
			},
		})
	}

	// Enqueue one more after the mix.
	q.Enqueue(Request{
		Label: "final-good",
		Execute: func(ctx context.Context) error {
			atomic.AddInt32(&goodCount, 1)
			return nil
		},
	})

	// Wait for the final request to complete.
	time.Sleep(2 * time.Second)
	if n := atomic.LoadInt32(&goodCount); n != 1 {
		t.Errorf("expected 1 final good request to execute, got %d", n)
	}
}

// mockError implements error for testing.
type mockError struct{ msg string }

func (e *mockError) Error() string { return e.msg }

// TestSharedLimiterCollectiveCap proves that when two queues share a single
// Limiter, the configured requests_per_minute is enforced as a collective cap
// across BOTH queues combined — not per-queue. With 120 RPM (500ms spacing),
// every pair of consecutive global starts must be ~500ms apart.
func TestSharedLimiterCollectiveCap(t *testing.T) {
	limiter := NewLimiter(120) // 120 RPM = 500ms spacing
	q1 := NewQueueWithLimiter(120, limiter)
	q2 := NewQueueWithLimiter(120, limiter)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	q1.Start(ctx)
	q2.Start(ctx)

	var mu sync.Mutex
	var starts []time.Time
	record := func() {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
	}

	// 3 requests on each queue, all enqueued immediately.
	for i := 0; i < 3; i++ {
		q1.Enqueue(Request{Label: "q1", Execute: func(context.Context) error { record(); return nil }})
		q2.Enqueue(Request{Label: "q2", Execute: func(context.Context) error { record(); return nil }})
	}

	// Wait for all 6 to complete.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(starts)
		mu.Unlock()
		if n >= 6 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 6 {
		t.Fatalf("expected 6 starts, got %d", len(starts))
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })

	// Consecutive global starts must be spaced ~500ms apart (collective cap).
	const minGap = 400 * time.Millisecond
	for i := 1; i < len(starts); i++ {
		gap := starts[i].Sub(starts[i-1])
		if gap < minGap {
			t.Errorf("gap between global starts %d-%d = %v, want >= %v (collective cap violated)", i-1, i, gap, minGap)
		}
	}
}

// TestSharedLimiterNoStarvation proves that a long-running request on one
// queue (a slow metadata-sync call) does not block a request on another queue
// (playback requestdl) that shares the same limiter.
func TestSharedLimiterNoStarvation(t *testing.T) {
	limiter := NewLimiter(600) // 100ms spacing
	qSlow := NewQueueWithLimiter(600, limiter)
	qFast := NewQueueWithLimiter(600, limiter)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	qSlow.Start(ctx)
	qFast.Start(ctx)

	syncStarted := make(chan struct{})
	release := make(chan struct{})
	fastDone := make(chan struct{})

	qSlow.Enqueue(Request{
		Label: "slow-sync",
		Execute: func(context.Context) error {
			close(syncStarted)
			<-release // hold the slow queue open
			return nil
		},
	})

	select {
	case <-syncStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("slow request never started")
	}

	qFast.Enqueue(Request{
		Label: "fast-requestdl",
		Execute: func(context.Context) error {
			close(fastDone)
			return nil
		},
	})

	// The fast request must complete while the slow sync request is still held
	// open — proving a long-running sync call cannot starve playback.
	select {
	case <-fastDone:
		// success
	case <-time.After(1 * time.Second):
		t.Fatal("fast request starved behind slow sync request")
	}

	close(release)
	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
	}
}

func TestRateLimiting(t *testing.T) {
	// 1200 req/min should process quickly (50ms spacing).
	q := NewQueue(1200)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	start := time.Now()
	var count int32
	for i := 0; i < 5; i++ {
		q.Enqueue(Request{
			Label: "rate",
			Execute: func(ctx context.Context) error {
				atomic.AddInt32(&count, 1)
				return nil
			},
		})
	}

	time.Sleep(400 * time.Millisecond)
	elapsed := time.Since(start)
	n := atomic.LoadInt32(&count)

	if n != 5 {
		t.Errorf("expected 5, got %d (elapsed: %v)", n, elapsed)
	}
}

func TestProcessLoopExitsOnContextCancel(t *testing.T) {
	q := NewQueue(100)
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)

	// Cancel before any items are enqueued.
	cancel()
	// Give processLoop time to notice ctx.Done() and exit.
	time.Sleep(50 * time.Millisecond)

	// Fill the channel buffer. With processLoop exited, items won't be
	// consumed, so the buffer should fill up.
	for i := 0; i < queueBufferSize; i++ {
		q.Enqueue(Request{
			Label: "post-cancel",
			Execute: func(ctx context.Context) error { return nil },
		})
	}

	// The buffer is now full. The next Enqueue would block forever
	// if processLoop truly exited. Use a goroutine with a short timeout.
	done := make(chan struct{})
	go func() {
		q.Enqueue(Request{
			Label: "should-block",
			Execute: func(ctx context.Context) error { return nil },
		})
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("processLoop still consuming items after context cancel — goroutine leak")
	case <-time.After(100 * time.Millisecond):
	}
}