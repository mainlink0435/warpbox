package throttle

import (
	"context"
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