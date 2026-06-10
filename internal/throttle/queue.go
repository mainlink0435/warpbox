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

// Request represents a queued API call.
type Request struct {
	Label    string
	Execute  func(ctx context.Context) error
}

// Queue is a rate-limited, blocking queue for TorBox API requests.
type Queue struct {
	mu         sync.Mutex
	cond       *sync.Cond
	queue      []Request
	rate       time.Duration
	lastCall   time.Time
	totalCalls int64
	callWindow []time.Time
}

// Stats returns throttle statistics for the landing page.
type Stats struct {
	TotalCalls       int64
	CallsLastMinute  int
	RequestsPerMinute int
}

// NewQueue creates a new throttle queue.
// requestsPerMinute sets the maximum sustained call rate.
func NewQueue(requestsPerMinute int) *Queue {
	q := &Queue{
		rate: time.Minute / time.Duration(requestsPerMinute),
	}
	q.cond = sync.NewCond(&q.mu)
	return q
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
		TotalCalls:        q.totalCalls,
		CallsLastMinute:   recent,
		RequestsPerMinute: int(time.Minute / q.rate),
	}
}

// Enqueue adds a request to the blocking queue.
func (q *Queue) Enqueue(r Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queue = append(q.queue, r)
	q.cond.Signal()
}

// processLoop runs in a goroutine, dequeuing and executing requests at the
// configured rate. It blocks when the queue is empty.
func (q *Queue) processLoop(ctx context.Context) {
	for {
		q.mu.Lock()
		for len(q.queue) == 0 {
			q.cond.Wait()
		}
		r := q.queue[0]
		q.queue = q.queue[1:]

		// Enforce minimum spacing between calls.
		elapsed := time.Since(q.lastCall)
		if elapsed < q.rate {
			q.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(q.rate - elapsed):
			}
		} else {
			q.mu.Unlock()
		}

		if err := r.Execute(ctx); err != nil {
			slog.Error("throttle request failed", "label", r.Label, "error", err)
		}

		q.mu.Lock()
		q.lastCall = time.Now()
		q.totalCalls++
		q.callWindow = append(q.callWindow, q.lastCall)
		// Keep the window trimmed to roughly the last 60 seconds.
		cutoff := q.lastCall.Add(-60 * time.Second)
		for len(q.callWindow) > 0 && q.callWindow[0].Before(cutoff) {
			q.callWindow = q.callWindow[1:]
		}
		q.mu.Unlock()
	}
}

// Start launches the processing loop in a background goroutine.
func (q *Queue) Start(ctx context.Context) {
	go q.processLoop(ctx)
}