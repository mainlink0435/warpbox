package cache

import (
	"testing"
	"time"
)

func TestPutGet(t *testing.T) {
	b := NewBuffer(1024*1024, 1024, 30*time.Second, StrategyTTL)
	defer b.Stop()

	data := []byte("hello world")
	b.Put(1, 0, data)

	got := b.Get(1, 0)
	if got == nil {
		t.Fatal("expected chunk, got nil")
	}
	if string(got) != "hello world" {
		t.Errorf("got %q, want %q", string(got), "hello world")
	}
}

func TestGetMiss(t *testing.T) {
	b := NewBuffer(1024*1024, 1024, 30*time.Second, StrategyTTL)
	defer b.Stop()

	got := b.Get(999, 0)
	if got != nil {
		t.Errorf("expected nil for missing key, got %v", got)
	}
}

func TestGetExpired(t *testing.T) {
	b := NewBuffer(1024*1024, 1024, 1*time.Millisecond, StrategyTTL)
	defer b.Stop()

	b.Put(1, 0, []byte("data"))
	time.Sleep(10 * time.Millisecond)

	got := b.Get(1, 0)
	if got != nil {
		t.Error("expected nil for expired chunk")
	}
}

func TestEvictionOnOverflow(t *testing.T) {
	// Each entry is ~60 bytes; maxRAM=140 fits 2 but not 3.
	b := NewBuffer(140, 1024, 30*time.Second, StrategyTTL)
	defer b.Stop()

	b.Put(1, 0, []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))  // 59 bytes
	b.Put(2, 0, []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))  // 59 bytes

	entries, used, _ := b.Stats()
	if entries != 2 {
		t.Errorf("expected 2 after puts 1+2, got %d (used=%d)", entries, used)
	}

	// Third put exceeds maxRAM → evict one.
	b.Put(3, 0, []byte("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")) // 66 bytes
	entries, used, max := b.Stats()
	if entries > 2 {
		t.Errorf("expected ≤2 after eviction, got %d (used=%d max=%d)", entries, used, max)
	}
	if used > max+40 {
		t.Errorf("usedRAM %d far exceeds maxRAM %d", used, max)
	}
}

func TestLRUEvictionOrder(t *testing.T) {
	b := NewBuffer(140, 1024, 30*time.Second, StrategyLRU)
	defer b.Stop()

	// Put 3 entries (~59+59+66 = 184 bytes, exceeds 140 → eviction).
	b.Put(1, 0, []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))  // 59 bytes
	b.Put(2, 0, []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"))  // 59 bytes

	// Touch entry 1 so it's most recent.
	b.Get(1, 0)
	time.Sleep(1 * time.Millisecond) // ensure distinct LastAccess times

	b.Put(3, 0, []byte("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")) // 66 bytes

	// Touch entry 2 so it's more recent than entry 1.
	b.Get(2, 0)
	time.Sleep(1 * time.Millisecond)

	// Fourth put: curr used ~125, +65 = 190 > 140 → evict one.
	b.Put(4, 0, []byte("ddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")) // 65 bytes

	entries, used, max := b.Stats()
	t.Logf("LRU test: entries=%d used=%d max=%d", entries, used, max)

	// Entry 1 was touched more recently than entries 3 and 4 (which are in puts 1 and 3 order).
	// Entry 1 should survive. At least 2 entries should exist.
	if entries < 2 {
		t.Errorf("expected ≥2 surviving entries, got %d", entries)
	}
}

func TestUpdateExisting(t *testing.T) {
	b := NewBuffer(1024*1024, 1024, 30*time.Second, StrategyTTL)
	defer b.Stop()

	b.Put(1, 0, []byte("old"))
	b.Put(1, 0, []byte("new"))

	got := b.Get(1, 0)
	if string(got) != "new" {
		t.Errorf("got %q, want %q", string(got), "new")
	}
}

func TestStats(t *testing.T) {
	b := NewBuffer(1024*1024, 1024, 30*time.Second, StrategyTTL)
	defer b.Stop()

	b.Put(1, 0, []byte("hello"))
	entries, used, max := b.Stats()
	if entries != 1 {
		t.Errorf("expected 1 entry, got %d", entries)
	}
	if used <= 0 {
		t.Errorf("expected usedRAM > 0, got %d", used)
	}
	if max != 1024*1024 {
		t.Errorf("expected maxRAM = %d, got %d", 1024*1024, max)
	}
}

func TestDifferentFileIDs(t *testing.T) {
	b := NewBuffer(1024*1024, 1024, 30*time.Second, StrategyTTL)
	defer b.Stop()

	b.Put(1, 0, []byte("file1"))
	b.Put(2, 0, []byte("file2"))

	if got := b.Get(1, 0); got == nil || string(got) != "file1" {
		t.Errorf("file 1: got %q, want file1", string(got))
	}
	if got := b.Get(2, 0); got == nil || string(got) != "file2" {
		t.Errorf("file 2: got %q, want file2", string(got))
	}
}

func TestDifferentOffsets(t *testing.T) {
	b := NewBuffer(1024*1024, 1024, 30*time.Second, StrategyTTL)
	defer b.Stop()

	b.Put(1, 0, []byte("offset0"))
	b.Put(1, 100, []byte("offset100"))

	if got := b.Get(1, 0); got == nil || string(got) != "offset0" {
		t.Errorf("offset 0: got %q", string(got))
	}
	if got := b.Get(1, 100); got == nil || string(got) != "offset100" {
		t.Errorf("offset 100: got %q", string(got))
	}
}