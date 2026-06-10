// Package metadata sync — periodic TorBox → SQLite sync worker.
//
// Runs on a configurable timer through the throttle queue. Flattens the
// nested torrent→files hierarchy from the TorBox API into a flat file
// records table in SQLite. This enables zero-API directory browsing.
package metadata

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/ben/warpbox/internal/throttle"
	"github.com/ben/warpbox/internal/torbox"
)

// SyncWorker periodically synchronises the TorBox file listing into SQLite.
type SyncWorker struct {
	store       *Store
	client      *torbox.Client
	queue       *throttle.Queue
	interval    time.Duration
	limit       int
	lastError   error
	lastSuccess time.Time
}

// SyncStatus describes the state of the most recent metadata sync.
type SyncStatus struct {
	LastSuccess time.Time // zero if never succeeded
	LastError   string    // empty if last sync succeeded
}

// Status returns the outcome of the most recent sync cycle.
func (w *SyncWorker) Status() SyncStatus {
	return SyncStatus{
		LastSuccess: w.lastSuccess,
		LastError:   errorString(w.lastError),
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	w.syncOnce(ctx)
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

	// Fetch torrents.
	w.queue.Enqueue(throttle.Request{
		Label: "metadata sync: ListTorrents",
		Execute: func(ctx context.Context) error {
			torrents, err := w.client.ListTorrents(ctx, torbox.ListFilesParams{
				BypassCache: false,
				Offset:      0,
				Limit:       w.limit,
			})
			torrentCh <- torrentResult{torrents, err}
			return err
		},
	})

	// Fetch Usenet downloads (shares the throttle queue).
	w.queue.Enqueue(throttle.Request{
		Label: "metadata sync: ListUsenet",
		Execute: func(ctx context.Context) error {
			usenet, err := w.client.ListUsenet(ctx, torbox.ListFilesParams{
				BypassCache: false,
				Offset:      0,
				Limit:       w.limit,
			})
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

	// Merge: torrents first, then usenet.
	var all []torbox.Torrent
	if torRes.torrents != nil {
		all = append(all, torRes.torrents...)
	}
	if usenetRes.usenet != nil {
		all = append(all, usenetRes.usenet...)
	}

	// Flatten all items into file records.
	var count int
	for _, t := range all {
		if t.DownloadState != "cached" && !t.DownloadPresent {
			continue
		}
		if len(t.Files) == 0 {
			slog.Debug("metadata sync: skipping item with no files", "id", t.ID, "name", t.Name)
			continue
		}

		for _, f := range t.Files {
			// Derive the virtual path from s3_path: strip the leading hash segment.
			// s3_path is always "hash/torrent_dir/file_name" for multi-file torrents
			// but can be "hash/file_name" for single-file torrents with no directory.
			// In the latter case, create a directory from the filename (minus ext)
			// to match traditional WebDAV behaviour.
			virtualPath := f.ShortName
			if idx := strings.IndexByte(f.S3Path, '/'); idx > 0 && idx+1 < len(f.S3Path) {
				rest := f.S3Path[idx+1:]
				// If rest has a second slash, it includes a directory — use directly.
				// Otherwise it's just a filename at root — wrap in a dir named after itself.
				if idx2 := strings.IndexByte(rest, '/'); idx2 >= 0 {
					virtualPath = rest
				} else {
					// Single file s3_path: "hash/filename.ext"
					// Place under a directory named after the file (minus extension).
					if dot := strings.LastIndexByte(rest, '.'); dot > 0 {
						virtualPath = rest[:dot] + "/" + rest
					} else {
						virtualPath = rest
					}
				}
			}

			rec := FileRecord{
				TorrentID: t.ID,
				FileID:    f.ID,
				Name:      f.ShortName,
				Path:      virtualPath,
				Size:      f.Size,
				MimeType:  f.MimeType,
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

	// Track sync status.
	var syncErr error
	if torRes.err != nil {
		syncErr = torRes.err
	} else if usenetRes.err != nil {
		syncErr = usenetRes.err
	}
	w.lastError = syncErr
	if syncErr == nil {
		w.lastSuccess = time.Now()
	}

	slog.Debug("metadata sync complete", "files_synced", count)
}
