// Package cache implements a JIT RAM buffer for video chunk look-aheads.
//
// File headers and media chunks are held in memory temporarily to serve rapid
// sequential byte-range requests from Plex. Unused chunks evaporate after a
// configurable TTL.
package cache

import (
	"sync"
	"time"
)

// Chunk is a single cached byte range.
type Chunk struct {
	Data      []byte
	ExpiresAt time.Time
}

// Buffer is a TTL-based in-memory cache keyed by (fileID, offset).
type Buffer struct {
	mu         sync.RWMutex
	entries    map[string]*Chunk
	maxRAM     int
	usedRAM    int
	ttl        time.Duration
	chunkSize  int
	stopCh     chan struct{}
}

// NewBuffer creates a new RAM buffer.
// maxRAM is the hard limit in bytes, ttl is the per-chunk lifetime.
func NewBuffer(maxRAM int, chunkSize int, ttl time.Duration) *Buffer {
	b := &Buffer{
		entries:   make(map[string]*Chunk),
		maxRAM:    maxRAM,
		ttl:       ttl,
		chunkSize: chunkSize,
		stopCh:    make(chan struct{}),
	}
	go b.evictionLoop()
	return b
}

// key builds a map key from a file identifier and byte offset.
func key(fileID int, offset int64) string {
	return string(rune(fileID)) + ":" + string(rune(offset)) // simplified
}

// Get retrieves a cached chunk, or nil if not present or expired.
func (b *Buffer) Get(fileID int, offset int64) []byte {
	b.mu.RLock()
	defer b.mu.RUnlock()

	entry, ok := b.entries[key(fileID, offset)]
	if !ok {
		return nil
	}
	if time.Now().After(entry.ExpiresAt) {
		return nil
	}
	return entry.Data
}

// Put stores a chunk in the buffer.
func (b *Buffer) Put(fileID int, offset int64, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	k := key(fileID, offset)
	// Evict oldest if we're over budget.
	for b.usedRAM+len(data) > b.maxRAM && len(b.entries) > 0 {
		b.evictOne()
	}

	b.entries[k] = &Chunk{
		Data:      data,
		ExpiresAt: time.Now().Add(b.ttl),
	}
	b.usedRAM += len(data)
}

// evictOne removes a single entry (simple map iteration order).
func (b *Buffer) evictOne() {
	for k, v := range b.entries {
		b.usedRAM -= len(v.Data)
		delete(b.entries, k)
		return
	}
}

// evictionLoop sweeps expired entries periodically.
func (b *Buffer) evictionLoop() {
	ticker := time.NewTicker(b.ttl / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			b.mu.Lock()
			now := time.Now()
			for k, v := range b.entries {
				if now.After(v.ExpiresAt) {
					b.usedRAM -= len(v.Data)
					delete(b.entries, k)
				}
			}
			b.mu.Unlock()
		case <-b.stopCh:
			return
		}
	}
}

// Stop halts the eviction loop.
func (b *Buffer) Stop() {
	close(b.stopCh)
}