// Package metadata provides a SQLite-backed store for the TorBox directory tree.
//
// Runs in WAL mode for high concurrent read performance. Browsing the
// virtual filesystem from Plex costs zero TorBox API calls.
package metadata

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// FileSource indicates the origin of a file record.
type FileSource int

const (
	SourceTorrent FileSource = 0
	SourceUsenet  FileSource = 1
)

// Store is a SQLite-backed metadata cache.
type Store struct {
	db *sql.DB

	// dbLockErrors counts "database is locked" errors for diagnostics.
	dbLockErrors atomic.Int64
}

// DBLockErrors returns the total number of database lock errors encountered.
func (s *Store) DBLockErrors() int64 {
	return s.dbLockErrors.Load()
}

// Ping checks whether the underlying SQLite database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// FileRecord represents a cached file entry from the TorBox directory.
// ItemID and FileID together identify a file in the TorBox API
// for CDN URL generation.
type FileRecord struct {
	ID           int64      `json:"id"`             // Internal auto-increment ID
	ItemID       int64      `json:"item_id"`        // TorBox parent ID (torrent or usenet ID, for CDN URL)
	FileID       int64      `json:"file_id"`        // TorBox file ID within parent (for CDN URL)
	Source       FileSource `json:"source"`         // SourceTorrent or SourceUsenet
	Name         string     `json:"name"`
	Path         string     `json:"path"`
	Size         int64      `json:"size"`
	MimeType     string     `json:"mime_type"`
	CreatedAt    string     `json:"created_at,omitempty"` // From TorBox API item.created_at
	CDNURL       string     `json:"cdn_url,omitempty"`
	CDNURLExpiry string     `json:"cdn_url_expires,omitempty"`
	SyncTag      int64      `json:"sync_tag,omitempty"`   // Sync batch tag for prune; 0 = unsynced
}

// isLockedError returns true if the error is a transient SQLite lock error.
func isLockedError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "SQLITE_LOCKED")
}

// Open opens (or creates) the SQLite database at the given path.
// WAL mode is enabled for high concurrency.
func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_cache_size=-8192")
	if err != nil {
		return nil, fmt.Errorf("opening sqlite database: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("pinging sqlite database: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("running migrations: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// migrate creates tables if they do not exist.
func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS files (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		item_id         INTEGER NOT NULL DEFAULT 0,
		file_id         INTEGER NOT NULL DEFAULT 0,
		source          INTEGER NOT NULL DEFAULT 0,
		name            TEXT    NOT NULL,
		path            TEXT    NOT NULL UNIQUE,
		size            INTEGER NOT NULL DEFAULT 0,
		mime_type       TEXT    NOT NULL DEFAULT '',
		cdn_url         TEXT    NOT NULL DEFAULT '',
		cdn_url_expires TEXT    NOT NULL DEFAULT '',
		created_at      TEXT    NOT NULL DEFAULT '',
		sync_tag        INTEGER NOT NULL DEFAULT 0,
		updated         TEXT    NOT NULL DEFAULT (datetime('now'))
	);
	CREATE INDEX IF NOT EXISTS idx_files_path ON files(path);
	CREATE INDEX IF NOT EXISTS idx_files_source_file_id ON files(source, file_id);
	CREATE INDEX IF NOT EXISTS idx_files_sync_tag ON files(sync_tag);
	CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '');
	CREATE TABLE IF NOT EXISTS stats (
		timestamp TEXT NOT NULL DEFAULT (datetime('now')),
		metric    TEXT NOT NULL,
		value     REAL NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_stats_metric_time ON stats(metric, timestamp);
	`
	_, err := s.db.Exec(schema)
	return err
}

// UpsertFile inserts or replaces a file record.
// The SyncTag field is used to tag records with the current sync batch
// so that PruneBySyncTag can delete records not touched by the latest sync.
func (s *Store) UpsertFile(f FileRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO files (item_id, file_id, source, name, path, size, mime_type, created_at, cdn_url, cdn_url_expires, sync_tag, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(path) DO UPDATE SET
			item_id         = excluded.item_id,
			file_id         = excluded.file_id,
			source          = excluded.source,
			name            = excluded.name,
			size            = excluded.size,
			mime_type       = excluded.mime_type,
			created_at      = excluded.created_at,
			cdn_url         = excluded.cdn_url,
			cdn_url_expires = excluded.cdn_url_expires,
			sync_tag        = excluded.sync_tag,
			updated         = excluded.updated
	`, f.ItemID, f.FileID, f.Source, f.Name, f.Path, f.Size, f.MimeType, f.CreatedAt, f.CDNURL, f.CDNURLExpiry, f.SyncTag)
	if isLockedError(err) {
		s.dbLockErrors.Add(1)
	}
	return err
}

// ListDir returns all files under the given virtual directory path.
func (s *Store) ListDir(prefix string) ([]FileRecord, error) {
	rows, err := s.db.Query(`
		SELECT id, item_id, file_id, source, name, path, size, mime_type, created_at FROM files
		WHERE path LIKE ? ORDER BY name
	`, prefix+"%")
	if err != nil {
		return nil, fmt.Errorf("querying files: %w", err)
	}
	defer rows.Close()

	var files []FileRecord
	for rows.Next() {
		var f FileRecord
		if err := rows.Scan(&f.ID, &f.ItemID, &f.FileID, &f.Source, &f.Name, &f.Path, &f.Size, &f.MimeType, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

// GetFileByPath returns a single file record by its virtual path.
// Returns nil if the path is not found.
func (s *Store) GetFileByPath(path string) (*FileRecord, error) {
	row := s.db.QueryRow(`
		SELECT id, item_id, file_id, source, name, path, size, mime_type, created_at, cdn_url, cdn_url_expires
		FROM files WHERE path = ?
	`, path)

	var f FileRecord
	err := row.Scan(&f.ID, &f.ItemID, &f.FileID, &f.Source, &f.Name, &f.Path, &f.Size, &f.MimeType, &f.CreatedAt, &f.CDNURL, &f.CDNURLExpiry)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying file by path: %w", err)
	}
	return &f, nil
}

// GetFileByFileID returns a single file record by its TorBox file ID and source.
// Returns nil if the (source, file_id) pair is not found.
func (s *Store) GetFileByFileID(source FileSource, fileID int64) (*FileRecord, error) {
	row := s.db.QueryRow(`
		SELECT id, item_id, file_id, source, name, path, size, mime_type, created_at, cdn_url, cdn_url_expires
		FROM files WHERE source = ? AND file_id = ? LIMIT 1
	`, source, fileID)

	var f FileRecord
	err := row.Scan(&f.ID, &f.ItemID, &f.FileID, &f.Source, &f.Name, &f.Path, &f.Size, &f.MimeType, &f.CreatedAt, &f.CDNURL, &f.CDNURLExpiry)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying file by file_id: %w", err)
	}
	return &f, nil
}

// SetCDNURL stores a CDN download URL for a file identified by its internal ID.
// Retries up to 3 times with exponential backoff (100ms, 200ms, 400ms) if the
// database is locked due to a concurrent write (e.g. sync worker prune).
func (s *Store) SetCDNURL(internalID int64, cdnURL string, expiresAt time.Time) error {
	start := time.Now()
	expires := expiresAt.UTC().Format(time.RFC3339)

	var err error
	for attempt := 0; attempt < 3; attempt++ {
		_, err = s.db.Exec(`
			UPDATE files SET cdn_url = ?, cdn_url_expires = ?, updated = datetime('now')
			WHERE id = ?
		`, cdnURL, expires, internalID)

		if err == nil {
			slog.Debug("db write duration", "method", "SetCDNURL", "duration_ms", time.Since(start).Milliseconds())
			return nil
		}

		if isLockedError(err) {
			s.dbLockErrors.Add(1)
			if attempt < 2 {
				backoff := time.Duration(100*(1<<attempt)) * time.Millisecond
				slog.Debug("SetCDNURL: database locked, retrying",
					"attempt", attempt+1,
					"backoff_ms", backoff.Milliseconds(),
				)
				time.Sleep(backoff)
				continue
			}
			slog.Warn("SetCDNURL: database locked, exhausted retries",
				"internal_id", internalID,
				"attempts", 3,
			)
		}
	}

	slog.Debug("db write duration", "method", "SetCDNURL", "duration_ms", time.Since(start).Milliseconds(), "error", err)
	return err
}

// GetCDNURL returns a cached CDN URL for a file identified by its internal ID.
func (s *Store) GetCDNURL(internalID int64) (string, error) {
	row := s.db.QueryRow(`
		SELECT cdn_url, cdn_url_expires FROM files WHERE id = ?
	`, internalID)

	var url, expires string
	if err := row.Scan(&url, &expires); err == sql.ErrNoRows {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("querying CDN URL: %w", err)
	}

	if url == "" {
		return "", nil
	}
	if expires == "" {
		return url, nil
	}

	expiryTime, err := time.Parse(time.RFC3339, expires)
	if err != nil {
		return "", nil
	}

	if time.Now().UTC().After(expiryTime) {
		return "", nil
	}

	return url, nil
}

// CountFiles returns the total number of file records in the store.
func (s *Store) CountFiles() (int, error) {
	row := s.db.QueryRow(`SELECT COUNT(*) FROM files`)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("counting files: %w", err)
	}
	return count, nil
}

// GetItemIDByFileID returns the item_id for a given (source, file_id) pair.
// This is needed because TorBox's requestdl endpoint requires the parent ID.
func (s *Store) GetItemIDByFileID(source FileSource, fileID int64) (int64, error) {
	row := s.db.QueryRow(`SELECT item_id FROM files WHERE source = ? AND file_id = ? LIMIT 1`, source, fileID)
	var tid int64
	if err := row.Scan(&tid); err == sql.ErrNoRows {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("querying item_id: %w", err)
	}
	return tid, nil
}

// GetNextSyncTag returns the next sync batch tag value and atomically
// increments the counter stored in a separate table. Each sync cycle
// reserves a unique tag so that records from that cycle can be identified
// and everything else can be pruned.
func (s *Store) GetNextSyncTag() (int64, error) {
	// Ensure the counter row exists.
	_, _ = s.db.Exec(`INSERT OR IGNORE INTO meta (key, value) VALUES ('sync_tag', '0')`)

	// Atomically increment and return the new value.
	var tag int64
	err := s.db.QueryRow(`UPDATE meta SET value = CAST(value AS INTEGER) + 1 RETURNING CAST(value AS INTEGER)`).Scan(&tag)
	if err != nil {
		return 0, fmt.Errorf("incrementing sync tag: %w", err)
	}
	return tag, nil
}

// PruneBySyncTag deletes all file records whose sync_tag does NOT match the
// given tag (i.e. they were not touched by this sync cycle). Records with
// sync_tag = 0 (legacy/unsynced) are also deleted.
//
// Deletes are performed in batches of 250 rows to avoid holding the SQLite
// writer lock for too long, which would starve concurrent writes from
// SetCDNURL. Returns the total number of records deleted across all batches.
func (s *Store) PruneBySyncTag(tag int64) (int, error) {
	if tag <= 0 {
		return 0, fmt.Errorf("refusing to prune with invalid sync tag %d", tag)
	}

	start := time.Now()
	const batchSize = 250
	var total int
	for {
		result, err := s.db.Exec(`
			DELETE FROM files WHERE id IN (
				SELECT id FROM files WHERE sync_tag != ? OR sync_tag = 0 LIMIT ?
			)
		`, tag, batchSize)
		if err != nil {
			if isLockedError(err) {
				s.dbLockErrors.Add(1)
			}
			slog.Debug("db write duration", "method", "PruneBySyncTag", "duration_ms", time.Since(start).Milliseconds(), "rows", total, "error", err)
			return total, fmt.Errorf("pruning by sync tag: %w", err)
		}

		n, err := result.RowsAffected()
		if err != nil {
			slog.Debug("db write duration", "method", "PruneBySyncTag", "duration_ms", time.Since(start).Milliseconds(), "rows", total, "error", err)
			return total, fmt.Errorf("rows affected after prune: %w", err)
		}

		total += int(n)
		if n == 0 {
			break
		}
	}

	slog.Debug("db write duration", "method", "PruneBySyncTag", "duration_ms", time.Since(start).Milliseconds(), "rows", total)
	return total, nil
}