// Package metadata provides a SQLite-backed store for the TorBox directory tree.
//
// Runs in WAL mode for high concurrent read performance. Browsing the
// virtual filesystem from Plex costs zero TorBox API calls.
package metadata

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

// Store is a SQLite-backed metadata cache.
type Store struct {
	db *sql.DB
}

// FileRecord represents a cached file entry from the TorBox directory.
type FileRecord struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	MimeType string `json:"mime_type"`
}

// Open opens (or creates) the SQLite database at the given path.
// WAL mode is enabled for high concurrency.
func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
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
		id        INTEGER PRIMARY KEY,
		name      TEXT    NOT NULL,
		path      TEXT    NOT NULL UNIQUE,
		size      INTEGER NOT NULL DEFAULT 0,
		mime_type TEXT    NOT NULL DEFAULT '',
		updated   TEXT    NOT NULL DEFAULT (datetime('now'))
	);
	CREATE INDEX IF NOT EXISTS idx_files_path ON files(path);
	`
	_, err := s.db.Exec(schema)
	return err
}

// UpsertFile inserts or replaces a file record.
func (s *Store) UpsertFile(f FileRecord) error {
	_, err := s.db.Exec(`
		INSERT INTO files (id, name, path, size, mime_type, updated)
		VALUES (?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(path) DO UPDATE SET
			id        = excluded.id,
			name      = excluded.name,
			size      = excluded.size,
			mime_type = excluded.mime_type,
			updated   = excluded.updated
	`, f.ID, f.Name, f.Path, f.Size, f.MimeType)
	return err
}

// ListDir returns all files under the given virtual directory path.
func (s *Store) ListDir(prefix string) ([]FileRecord, error) {
	rows, err := s.db.Query(`
		SELECT id, name, path, size, mime_type FROM files
		WHERE path LIKE ? ORDER BY name
	`, prefix+"%")
	if err != nil {
		return nil, fmt.Errorf("querying files: %w", err)
	}
	defer rows.Close()

	var files []FileRecord
	for rows.Next() {
		var f FileRecord
		if err := rows.Scan(&f.ID, &f.Name, &f.Path, &f.Size, &f.MimeType); err != nil {
			return nil, fmt.Errorf("scanning row: %w", err)
		}
		files = append(files, f)
	}
	return files, rows.Err()
}