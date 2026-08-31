// Package throttle implements a blocking request queue with a token-bucket rate limiter.
//
// Warpbox never fails fast. Burst traffic from Plex is queued and trickled
// to the TorBox API at a safe rate below 300 requests per minute.
package throttle

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const queueBufferSize = 1024

// Request represents a queued API call.
type Request struct {
	Label    string
	Execute  func(ctx context.Context) error
}

// Limiter paces the *start* of API calls. Multiple queues can share a single
// Limiter so the aggregate call start rate across all of them never exceeds
// requestsPerMinute, while each queue still runs its own processLoop. This is
// what keeps the configured rate a true collective cap across the metadata
// sync and playback (requestdl) queues, instead of a per-queue limit.
type Limiter struct {
	mu       sync.Mutex
	rate     time.Duration
	lastCall time.Time
}

// NewLimiter creates a shared rate limiter that paces call starts.
// requestsPerMinute sets the maximum collective start rate.
func NewLimiter(requestsPerMinute int) *Limiter {
	return &Limiter{
		rate: time.Minute / time.Duration(requestsPerMinute),
	}
}

// Wait blocks until a start slot is available, then reserves it. The pacing is
// start-to-start: consecutive starts (across all queues sharing this limiter)
// are spaced at least rate apart. Returns promptly on context cancellation.
func (l *Limiter) Wait(ctx context.Context) {
	for {
		l.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(l.lastCall)
		if elapsed >= l.rate {
			l.lastCall = now
			l.mu.Unlock()
			return
		}
		wait := l.rate - elapsed
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Queue is a rate-limited, blocking queue for TorBox API requests.
type Queue struct {
	mu      sync.Mutex
	items   chan Request
	limiter *Limiter
	rate    time.Duration
	// Stats counters (per-queue; the server aggregates across queues).
	totalCalls int64
	callWindow []time.Time

	successfulCalls int64
	failedCalls     int64
	http429Calls    int64
}

// Stats returns throttle statistics for the landing page.
type Stats struct {
	TotalCalls        int64
	SuccessfulCalls   int64
	FailedCalls       int64
	HTTP429Calls      int64
	CallsLastMinute   int
	RequestsPerMinute int
}

// NewQueue creates a new self-paced throttle queue.
// requestsPerMinute sets the maximum sustained call rate.
func NewQueue(requestsPerMinute int) *Queue {
	return NewQueueWithLimiter(requestsPerMinute, nil)
}

// NewQueueWithLimiter creates a throttle queue paced by the given limiter.
// If limiter is nil, the queue gets its own private limiter. Sharing a limiter
// between queues enforces the collective requestsPerMinute across all of them.
func NewQueueWithLimiter(requestsPerMinute int, shared *Limiter) *Queue {
	if shared == nil {
		shared = NewLimiter(requestsPerMinute)
	}
	return &Queue{
		items:   make(chan Request, queueBufferSize),
		limiter: shared,
		rate:    time.Minute / time.Duration(requestsPerMinute),
	}
}

// Stats returns current throttle statistics.
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Count calls in the last 60 seconds.
	now := time.Now()
	cutoff := now.Add(-60 * time.Second)
	recent := 0
	for _, t := range q.callWindow {
		if t.After(cutoff) {
			recent++
		}
	}

	return Stats{
		TotalCalls:         q.totalCalls,
		SuccessfulCalls:    q.successfulCalls,
		FailedCalls:        q.failedCalls,
		HTTP429Calls:       q.http429Calls,
		CallsLastMinute:    recent,
		RequestsPerMinute:  int(time.Minute / q.rate),
	}
}

// Enqueue adds a request to the blocking queue.
// If the queue buffer is full, Enqueue blocks until space is available.
func (q *Queue) Enqueue(r Request) {
	q.items <- r
}

// processLoop runs in a goroutine, receiving and executing requests at the
// configured rate. It blocks on the channel when the queue is empty and
// exits cleanly when the context is cancelled.
func (q *Queue) processLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-q.items:
			// Pace the start through the (possibly shared) limiter. This blocks
			// only while acquiring a start slot; a long-running Execute in one
			// queue never delays another queue sharing the same limiter.
			q.limiter.Wait(ctx)
			// If the context was cancelled while waiting for a start slot, exit
			// the loop without running the request (matches pre-split behaviour
			// of dropping pending work on shutdown).
			if ctx.Err() != nil {
				return
			}

			err := r.Execute(ctx)

			q.mu.Lock()
			q.totalCalls++
			if err != nil {
				q.failedCalls++
				// Log throttle-level failure at DEBUG only. The caller (e.g. handleGet)
				// owns the ERROR-level log with full context (torrent_id, file_id, etc.).
				slog.Debug("throttle request failed", "label", r.Label, "error", err)
			} else {
				q.successfulCalls++
			}
			q.callWindow = append(q.callWindow, time.Now())
			// Keep the window trimmed to roughly the last 60 seconds.
			cutoff := time.Now().Add(-60 * time.Second)
			for len(q.callWindow) > 0 && q.callWindow[0].Before(cutoff) {
				q.callWindow = q.callWindow[1:]
			}
			q.mu.Unlock()
		}
	}
}

// Record429 increments the 429 counter under lock.
func (q *Queue) Record429() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.http429Calls++
}

// Start launches the processing loop in a background goroutine.
func (q *Queue) Start(ctx context.Context) {
	go q.processLoop(ctx)
}
