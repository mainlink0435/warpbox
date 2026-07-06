// Package metadata sync — periodic TorBox → SQLite sync worker.
//
// Runs on a configurable timer through the throttle queue. Flattens the
// nested torrent→files hierarchy from the TorBox API into a flat file
// records table in SQLite. This enables zero-API directory browsing.
package metadata

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/mainlink0435/warpbox/internal/throttle"
	"github.com/mainlink0435/warpbox/internal/torbox"
)

// SyncWorker periodically synchronises the TorBox file listing into SQLite.
type SyncWorker struct {
	store          *Store
	client         *torbox.Client
	queue          *throttle.Queue
	interval       time.Duration
	listPageSize   int
	bypassCache    bool
	retryAttempts  int
	retryBackoff   time.Duration
	lastError      error
	lastSuccess    time.Time

	// Hooks for library change detection.
	OnItemsAdded   func(itemNames []string)
	OnItemsRemoved func(itemNames []string)

	mu        sync.Mutex
	parentCtx context.Context
	cancel    context.CancelFunc
	loopDone  chan struct{}
}

// SyncStatus describes the state of the most recent metadata sync.
type SyncStatus struct {
	LastSuccess time.Time // zero if never succeeded
	LastError   string    // empty if last sync succeeded
}

// Status returns the outcome of the most recent sync cycle.
func (w *SyncWorker) Status() SyncStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return SyncStatus{
		LastSuccess: w.lastSuccess,
		LastError:   errorString(w.lastError),
	}
}

// NewSyncWorker creates a new metadata sync worker.
// interval is how often to sync (e.g. 5 minutes).
// listPageSize is the per-request page window when paginating mylist API calls.
// retryAttempts is the max number of retries for transient API errors.
// retryBackoff is the base backoff duration (exponential: 1x, 2x, 4x).
func NewSyncWorker(store *Store, client *torbox.Client, queue *throttle.Queue, interval time.Duration, listPageSize int, bypassCache bool, retryAttempts int, retryBackoff time.Duration) *SyncWorker {
	return &SyncWorker{
		store:          store,
		client:         client,
		queue:          queue,
		interval:       interval,
		listPageSize:   listPageSize,
		bypassCache:    bypassCache,
		retryAttempts:  retryAttempts,
		retryBackoff:   retryBackoff,
	}
}

// Start begins the periodic sync loop. Blocks until ctx is cancelled.
// Stores ctx as the parent context so Restart can derive fresh contexts from it.
func (w *SyncWorker) Start(ctx context.Context) {
	w.mu.Lock()
	w.parentCtx = ctx
	runCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	loopDone := make(chan struct{})
	w.loopDone = loopDone
	w.mu.Unlock()

	w.runLoop(runCtx)
	close(loopDone)
}

// runLoop contains the core ticker loop extracted from Start so that Stop
// and Restart can manage the lifecycle without duplicating logic.
func (w *SyncWorker) runLoop(ctx context.Context) {
	slog.Info("metadata sync worker started", "interval_minutes", w.interval.Minutes())

	w.syncOnce(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.syncOnce(ctx)
		case <-ctx.Done():
			slog.Info("metadata sync worker stopped")
			return
		}
	}
}

// Stop cancels the current sync loop and waits for it to finish.
// Safe to call multiple times or before Start.
func (w *SyncWorker) Stop() {
	w.mu.Lock()
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
	loopDone := w.loopDone
	w.mu.Unlock()

	if loopDone != nil {
		select {
		case <-loopDone:
		case <-time.After(90 * time.Second):
			slog.Warn("sync worker stop timed out")
		}
	}
}

// Restart stops the current sync loop and starts a fresh one.
// The new loop derives its context from the parent context stored at startup,
// so it still respects process-level shutdown (SIGINT/SIGTERM).
func (w *SyncWorker) Restart() {
	slog.Info("sync worker restart requested")
	w.Stop()

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.parentCtx == nil {
		slog.Warn("sync worker restart skipped: never started")
		return
	}

	ctx, cancel := context.WithCancel(w.parentCtx)
	w.cancel = cancel
	loopDone := make(chan struct{})
	w.loopDone = loopDone
	go func() {
		w.runLoop(ctx)
		close(loopDone)
	}()
	slog.Info("sync worker restarted")
}

// SyncNow triggers an immediate out-of-cycle sync using a fresh background context.
func (w *SyncWorker) SyncNow() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	w.syncOnce(ctx)
}

// sanitizePathSegment removes characters that are invalid or problematic in
// filesystem paths across Windows and Linux: \ / : * ? " < > | and &.
// These are replaced with an underscore. The function preserves valid Unicode
// characters including spaces, dots, and hyphens.
func sanitizePathSegment(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|', '&':
			// & is sanitized because it can cause issues in filesystem paths
			// and is stripped by the official TorBox WebDAV.
			if unicode.IsPrint(r) {
				b.WriteRune('_')
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fireChangeHooks compares old and new item sets and calls the registered
// OnItemsAdded and OnItemsRemoved hooks with the directory names of changed items.
func (w *SyncWorker) fireChangeHooks(oldItems, newItems []ItemDir) {
	type key struct {
		itemID int64
		source FileSource
	}
	oldSet := make(map[key]string, len(oldItems))
	for _, it := range oldItems {
		oldSet[key{it.ItemID, it.Source}] = it.Dir
	}
	newSet := make(map[key]string, len(newItems))
	for _, it := range newItems {
		newSet[key{it.ItemID, it.Source}] = it.Dir
	}

	var added, removed []string
	for k, dir := range newSet {
		if _, exists := oldSet[k]; !exists {
			added = append(added, dir)
		}
	}
	for k, dir := range oldSet {
		if _, exists := newSet[k]; !exists {
			removed = append(removed, dir)
		}
	}

	if len(added) > 0 && w.OnItemsAdded != nil {
		slog.Info("metadata sync: items added", "count", len(added), "items", added)
		w.OnItemsAdded(added)
	}
	if len(removed) > 0 && w.OnItemsRemoved != nil {
		slog.Info("metadata sync: items removed", "count", len(removed), "items", removed)
		w.OnItemsRemoved(removed)
	}
}

// errorString converts an error to a string, returning "" for nil.
func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// syncOnce performs a single sync cycle through the throttle queue.
func (w *SyncWorker) syncOnce(ctx context.Context) {
	slog.Debug("metadata sync: starting")

	// Snapshot current items for change detection.
	var oldItems []ItemDir
	if w.OnItemsAdded != nil || w.OnItemsRemoved != nil {
		var err error
		oldItems, err = w.store.ListItemDirs()
		if err != nil {
			slog.Warn("metadata sync: failed to snapshot items for change detection", "error", err)
		}
	}

	// Record GC cycles before sync to measure allocation pressure.
	var gcStart uint32
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		gcStart = mem.NumGC
	}

	type torrentResult struct {
		torrents []torbox.Torrent
		err      error
	}
	type usenetResult struct {
		usenet []torbox.Torrent
		err    error
	}

	torrentCh := make(chan torrentResult, 1)
	usenetCh := make(chan usenetResult, 1)

	// Fetch torrents with retry for transient errors (502, timeout, HTML error pages).
	w.queue.Enqueue(throttle.Request{
		Label: "metadata sync: ListTorrents",
		Execute: func(ctx context.Context) error {
			var torrents []torbox.Torrent
			var err error
			for attempt := 0; attempt <= w.retryAttempts; attempt++ {
				if attempt > 0 {
					select {
					case <-ctx.Done():
						torrentCh <- torrentResult{nil, ctx.Err()}
						return ctx.Err()
					case <-time.After(w.retryBackoff * time.Duration(1<<(attempt-1))):
					}
				}
			torrents, err = w.client.ListTorrents(ctx, torbox.ListFilesParams{
				BypassCache: w.bypassCache,
				Offset:      0,
				PageSize:    w.listPageSize,
			})
				if err == nil || !torbox.IsRetryable(err) {
					break
				}
				slog.Debug("metadata sync: ListTorrents failed, retrying",
					"attempt", attempt+1,
					"error", err,
				)
			}
			torrentCh <- torrentResult{torrents, err}
			return err
		},
	})

	// Fetch Usenet downloads with retry for transient errors.
	w.queue.Enqueue(throttle.Request{
		Label: "metadata sync: ListUsenet",
		Execute: func(ctx context.Context) error {
			var usenet []torbox.Torrent
			var err error
			for attempt := 0; attempt <= w.retryAttempts; attempt++ {
				if attempt > 0 {
					select {
					case <-ctx.Done():
						usenetCh <- usenetResult{nil, ctx.Err()}
						return ctx.Err()
					case <-time.After(w.retryBackoff * time.Duration(1<<(attempt-1))):
					}
				}
			usenet, err = w.client.ListUsenet(ctx, torbox.ListFilesParams{
				BypassCache: w.bypassCache,
				Offset:      0,
				PageSize:    w.listPageSize,
			})
				if err == nil || !torbox.IsRetryable(err) {
					break
				}
				slog.Debug("metadata sync: ListUsenet failed, retrying",
					"attempt", attempt+1,
					"error", err,
				)
			}
			usenetCh <- usenetResult{usenet, err}
			return err
		},
	})

	torRes := <-torrentCh
	if torRes.err != nil {
		slog.Error("metadata sync: torrents failed", "error", torRes.err)
		// Don't return — usenet may still succeed.
	}

	usenetRes := <-usenetCh
	if usenetRes.err != nil {
		slog.Error("metadata sync: usenet failed", "error", usenetRes.err)
	}

	// Reserve a sync batch tag so we can prune stale records later.
	// This tag is atomically incremented and stored in the meta table.
	syncTag, err := w.store.GetNextSyncTag()
	if err != nil {
		slog.Error("metadata sync: failed to get sync tag", "error", err)
		return
	}

	// Flatten torrent items into file records with SourceTorrent.
	var count int
	for _, t := range torRes.torrents {
		if t.DownloadState != "cached" && !t.DownloadPresent {
			continue
		}
		if len(t.Files) == 0 {
			slog.Debug("metadata sync: skipping torrent with no files", "id", t.ID, "name", t.Name)
			continue
		}

		for _, f := range t.Files {
			rec := buildFileRecord(t.ID, f, syncTag, SourceTorrent, t.CreatedAt)
			if err := w.store.UpsertFile(rec); err != nil {
				slog.Error("metadata sync: upsert failed",
					"file_id", f.ID,
					"path", rec.Path,
					"error", err,
				)
				continue
			}
			count++
		}
	}

	// Flatten usenet items into file records with SourceUsenet.
	for _, u := range usenetRes.usenet {
		if u.DownloadState != "cached" && !u.DownloadPresent {
			continue
		}
		if len(u.Files) == 0 {
			slog.Debug("metadata sync: skipping usenet item with no files", "id", u.ID, "name", u.Name)
			continue
		}

		for _, f := range u.Files {
			rec := buildFileRecord(u.ID, f, syncTag, SourceUsenet, u.CreatedAt)
			if err := w.store.UpsertFile(rec); err != nil {
				slog.Error("metadata sync: upsert failed",
					"file_id", f.ID,
					"path", rec.Path,
					"error", err,
				)
				continue
			}
			count++
		}
	}

	// Track sync status.
	var syncErr error
	if torRes.err != nil {
		syncErr = torRes.err
	} else if usenetRes.err != nil {
		syncErr = usenetRes.err
	}
	w.mu.Lock()
	w.lastError = syncErr
	if syncErr == nil {
		w.lastSuccess = time.Now()
	}
	w.mu.Unlock()

	// Prune stale records using the sync tag. Records with sync_tag != the
	// current tag were not touched by this sync and are safe to remove.
	// We always prune, even on partial fetch failure, to avoid accumulating
	// orphaned entries for torrents that have been removed from the account.
	if syncTag > 0 && (torRes.err == nil || usenetRes.err == nil) {
		deleted, pruneErr := w.store.PruneBySyncTag(syncTag)
		if pruneErr != nil {
			slog.Error("metadata sync: prune failed", "error", pruneErr)
		} else if deleted > 0 {
			slog.Info("metadata sync: pruned stale records", "count", deleted)
		}
	}

	// Detect added/removed items and fire hooks.
	if len(oldItems) > 0 && (w.OnItemsAdded != nil || w.OnItemsRemoved != nil) {
		newItems, err := w.store.ListItemDirs()
		if err != nil {
			slog.Warn("metadata sync: failed to fetch items for change detection", "error", err)
		} else if len(newItems) > 0 {
			w.fireChangeHooks(oldItems, newItems)
		}
	}

	// Log GC cycles that fired during this sync, if any.
	if gcStart > 0 {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		gcDelta := mem.NumGC - gcStart
		if gcDelta > 0 {
			slog.Debug("metadata sync: gc cycles during sync", "gc_delta", gcDelta)
		}
	}

	slog.Debug("metadata sync complete", "files_synced", count)
}

// buildFileRecord creates a FileRecord from a TorBox item and file.
func buildFileRecord(itemID int64, f torbox.TorrentFile, syncTag int64, source FileSource, createdAt string) FileRecord {
	// Derive the virtual path from s3_path: strip the leading hash segment.
	// s3_path is always "hash/torrent_dir/file_name" for multi-file torrents
	// but can be "hash/file_name" for single-file torrents with no directory.
	// Single-file torrents are placed directly at root level (no wrapper dir).
	var virtualPath string
	if idx := strings.IndexByte(f.S3Path, '/'); idx > 0 && idx+1 < len(f.S3Path) {
		rest := f.S3Path[idx+1:]
		if idx2 := strings.IndexByte(rest, '/'); idx2 >= 0 {
			// Multi-file torrent with a directory: "hash/dir/file"
			// Sanitize each path segment.
			segments := strings.Split(rest, "/")
			for i, seg := range segments {
				segments[i] = sanitizePathSegment(seg)
			}
			virtualPath = strings.Join(segments, "/")
		} else {
			// Single file s3_path: "hash/filename.ext"
			// Place directly at root level (no wrapper directory).
			virtualPath = sanitizePathSegment(rest)
		}
	} else {
		// Fallback: no slash in s3_path at all — use sanitized ShortName.
		virtualPath = sanitizePathSegment(f.ShortName)
	}

	return FileRecord{
		ItemID:    itemID,
		FileID:    f.ID,
		Source:    source,
		Name:      sanitizePathSegment(f.ShortName),
		Path:      virtualPath,
		Size:      f.Size,
		MimeType:  f.MimeType,
		CreatedAt: createdAt,
		SyncTag:   syncTag,
	}
}