// Package cache implements a JIT RAM buffer for video chunk look-aheads.
//
// File headers and media chunks are held in memory temporarily to serve rapid
// sequential byte-range requests from Plex. Unused chunks evaporate based on
// either a TTL or an LRU strategy, depending on the config.
package cache

import (
	"log/slog"
	"sync"
	"time"
)

// EvictionStrategy defines how chunks are evicted when max_ram_mb is exceeded.
type EvictionStrategy string

const (
	StrategyTTL EvictionStrategy = "ttl" // Evict after TTL seconds of inactivity
	StrategyLRU EvictionStrategy = "lru" // Evict LRU chunks when over budget
)

// Chunk is a single cached byte range.
type Chunk struct {
	Data      []byte
	ExpiresAt time.Time
	LastAccess time.Time // used by LRU strategy
}

// Buffer is a TTL- or LRU-based in-memory cache keyed by (fileID, offset).
type Buffer struct {
	mu         sync.RWMutex
	entries    map[string]*Chunk
	maxRAM     int
	usedRAM    int
	ttl        time.Duration
	chunkSize  int
	strategy   EvictionStrategy
	stopCh     chan struct{}
}

// NewBuffer creates a new RAM buffer with the given eviction strategy.
// maxRAM is the hard limit in bytes, ttl is the per-chunk lifetime for TTL strategy.
func NewBuffer(maxRAM int, chunkSize int, ttl time.Duration, strategy EvictionStrategy) *Buffer {
	b := &Buffer{
		entries:   make(map[string]*Chunk),
		maxRAM:    maxRAM,
		ttl:       ttl,
		chunkSize: chunkSize,
		strategy:  strategy,
		stopCh:    make(chan struct{}),
	}
	go b.evictionLoop()
	return b
}

// key builds a map key from a file identifier and byte offset.
func key(fileID int, offset int64) string {
	return string(rune(fileID)) + ":" + string(rune(offset))
}

// Get retrieves a cached chunk, or nil if not present or expired.
func (b *Buffer) Get(fileID int, offset int64) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()

	entry, ok := b.entries[key(fileID, offset)]
	if !ok {
		return nil
	}

	if b.strategy == StrategyTTL && time.Now().After(entry.ExpiresAt) {
		b.usedRAM -= len(entry.Data)
		delete(b.entries, key(fileID, offset))
		return nil
	}

	// Touch for LRU tracking.
	entry.LastAccess = time.Now()
	return entry.Data
}

// Put stores a chunk in the buffer.
func (b *Buffer) Put(fileID int, offset int64, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()

	k := key(fileID, offset)

	// If already cached, just update the data.
	if existing, ok := b.entries[k]; ok {
		b.usedRAM -= len(existing.Data)
		b.usedRAM += len(data)
		existing.Data = data
		existing.ExpiresAt = time.Now().Add(b.ttl)
		existing.LastAccess = time.Now()
		return
	}

	// Evict until we have room.
	for b.usedRAM+len(data) > b.maxRAM && len(b.entries) > 0 {
		b.evictOne()
	}

	b.entries[k] = &Chunk{
		Data:       data,
		ExpiresAt:  time.Now().Add(b.ttl),
		LastAccess: time.Now(),
	}
	b.usedRAM += len(data)
}

// evictOne removes a single entry based on the configured strategy.
func (b *Buffer) evictOne() {
	switch b.strategy {
	case StrategyLRU:
		b.evictLRU()
	default:
		b.evictArbitrary()
	}
}

// evictLRU finds and removes the least recently used chunk.
func (b *Buffer) evictLRU() {
	var oldestKey string
	var oldestTime time.Time
	first := true

	for k, v := range b.entries {
		if first || v.LastAccess.Before(oldestTime) {
			oldestKey = k
			oldestTime = v.LastAccess
			first = false
		}
	}

	if oldestKey != "" {
		b.usedRAM -= len(b.entries[oldestKey].Data)
		delete(b.entries, oldestKey)
	}
}

// evictArbitrary removes any single entry (simple map iteration order).
// Used for TTL strategy when the eviction loop hasn't kept up.
func (b *Buffer) evictArbitrary() {
	for k, v := range b.entries {
		b.usedRAM -= len(v.Data)
		delete(b.entries, k)
		return
	}
}

// evictionLoop sweeps expired entries periodically for TTL strategy.
func (b *Buffer) evictionLoop() {
	ticker := time.NewTicker(b.ttl / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if b.strategy == StrategyTTL {
				b.mu.Lock()
				now := time.Now()
				for k, v := range b.entries {
					if now.After(v.ExpiresAt) {
						b.usedRAM -= len(v.Data)
						delete(b.entries, k)
					}
				}
				b.mu.Unlock()
			}
		case <-b.stopCh:
			return
		}
	}
}

// Stats returns current cache statistics.
func (b *Buffer) Stats() (entries, usedRAM, maxRAM int) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.entries), b.usedRAM, b.maxRAM
}

// Stop halts the eviction loop.
func (b *Buffer) Stop() {
	close(b.stopCh)
}

// LogStats logs cache statistics at debug level.
func (b *Buffer) LogStats() {
	entries, used, max := b.Stats()
	if used > 0 {
		slog.Debug("cache stats", "entries", entries, "used_mb", used/(1024*1024), "max_mb", max/(1024*1024))
	}
}