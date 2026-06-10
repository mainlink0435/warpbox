// Package metadata sync — periodic TorBox → SQLite sync worker.
//
// Runs on a configurable timer through the throttle queue. Flattens the
// nested torrent→files hierarchy from the TorBox API into a flat file
// records table in SQLite. This enables zero-API directory browsing.
package metadata

import (
	"context"
	"log/slog"
	"path"
	"time"

	"github.com/ben/warpbox/internal/throttle"
	"github.com/ben/warpbox/internal/torbox"
)

// SyncWorker periodically synchronises the TorBox file listing into SQLite.
type SyncWorker struct {
	store    *Store
	client   *torbox.Client
	queue    *throttle.Queue
	interval time.Duration
	limit    int
}

// NewSyncWorker creates a new metadata sync worker.
// interval is how often to sync (e.g. 5 minutes).
// limit is the max number of files to fetch per sync.
func NewSyncWorker(store *Store, client *torbox.Client, queue *throttle.Queue, interval time.Duration, limit int) *SyncWorker {
	return &SyncWorker{
		store:    store,
		client:   client,
		queue:    queue,
		interval: interval,
		limit:    limit,
	}
}

// Start begins the periodic sync loop. Blocks until ctx is cancelled.
func (w *SyncWorker) Start(ctx context.Context) {
	slog.Info("metadata sync worker started", "interval_minutes", w.interval.Minutes())

	// Run an immediate sync on startup.
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

// SyncNow triggers an immediate out-of-cycle sync using a fresh background context.
func (w *SyncWorker) SyncNow() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	w.syncOnce(ctx)
}

// syncOnce performs a single sync cycle through the throttle queue.
func (w *SyncWorker) syncOnce(ctx context.Context) {
	slog.Debug("metadata sync: starting")

	type result struct {
		torrents []torbox.Torrent
		err      error
	}
	resCh := make(chan result, 1)

	w.queue.Enqueue(throttle.Request{
		Label: "metadata sync: ListFiles",
		Execute: func(ctx context.Context) error {
			torrents, err := w.client.ListFiles(ctx, torbox.ListFilesParams{
				BypassCache: false,
				Offset:      0,
				Limit:       w.limit,
			})
			resCh <- result{torrents, err}
			return err
		},
	})

	res := <-resCh
	if res.err != nil {
		slog.Error("metadata sync failed", "error", res.err)
		return
	}

	// Flatten torrents into file records.
	var count int
	for _, t := range res.torrents {
		// Only sync torrents that are cached (ready for streaming).
		if t.DownloadState != "cached" && !t.DownloadPresent {
			continue
		}

		for _, f := range t.Files {
			// Build virtual path: torrent_name/filename
			virtualPath := path.Join(t.Name, f.Name)

			rec := FileRecord{
				TorrentID: t.ID,
				FileID:    f.ID,
				Name:      f.Name,
				Path:      virtualPath,
				Size:      f.Size,
				MimeType: f.MimeType,
			}
			if err := w.store.UpsertFile(rec); err != nil {
				slog.Error("metadata sync: upsert failed",
					"file_id", f.ID,
					"path", virtualPath,
					"error", err,
				)
				continue
			}
			count++
		}
	}

	slog.Debug("metadata sync complete", "files_synced", count)
}