package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run internal/tools/dbinspect/main.go <path-to-db>")
		os.Exit(1)
	}
	dbPath := os.Args[1]

	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	fmt.Printf("Database: %s (%s)\n", dbPath, filepath.Base(dbPath))

	// File info
	fmt.Println("\n=== File Stats ===")
	row := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(size), 0), MIN(size), MAX(size), COUNT(DISTINCT substring(path, 1, instr(path || '/', '/') - 1)) FROM files`)
	var count, totalSize, minSize, maxSize, torrentCount int64
	if err := row.Scan(&count, &totalSize, &minSize, &maxSize, &torrentCount); err != nil {
		fmt.Fprintf(os.Stderr, "query error: %v\n", err)
	} else {
		var distinct int64
		db.QueryRow(`SELECT COUNT(DISTINCT path) FROM files`).Scan(&distinct)
		fmt.Printf("  Files:         %d total / %d unique (by path)\n", count, distinct)
		fmt.Printf("  Torrents:      %d\n", torrentCount)
		fmt.Printf("  Total size:    %s\n", formatSize(totalSize))
		fmt.Printf("  Min file size: %s\n", formatSize(minSize))
		fmt.Printf("  Max file size: %s\n", formatSize(maxSize))
	}

	// CDN URL stats
	fmt.Println("\n=== CDN Cache Stats ===")
	row = db.QueryRow(`SELECT COUNT(*), COUNT(CASE WHEN cdn_url != '' THEN 1 END), COUNT(CASE WHEN cdn_url_expires != '' AND cdn_url_expires < datetime('now') THEN 1 END) FROM files`)
	var total, cached, expired int
	if err := row.Scan(&total, &cached, &expired); err != nil {
		fmt.Fprintf(os.Stderr, "query error: %v\n", err)
	} else {
		fmt.Printf("  Cached URLs:   %d / %d\n", cached, total)
		fmt.Printf("  Expired URLs:  %d\n", expired)
	}

	// Sample entries
	fmt.Println("\n=== Sample Records (first 5) ===")
	rows, err := db.Query(`SELECT id, item_id, file_id, name, path, size, mime_type FROM files LIMIT 5`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query error: %v\n", err)
	} else {
		defer rows.Close()
		for rows.Next() {
			var id, iid, fid, size int64
			var name, path, mime string
			if err := rows.Scan(&id, &iid, &fid, &name, &path, &size, &mime); err != nil {
				fmt.Fprintf(os.Stderr, "scan error: %v\n", err)
				continue
			}
			fmt.Printf("  [%d] i=%d f=%d %s (%s) — %s\n", id, iid, fid, name, mime, formatSize(size))
			fmt.Printf("       path=%s\n", path)
		}
	}

	// Check for anomalies
	fmt.Println("\n=== Anomaly Check ===")
	var nullTorrents int64
	db.QueryRow(`SELECT COUNT(*) FROM files WHERE item_id = 0`).Scan(&nullTorrents)
	if nullTorrents > 0 {
		fmt.Printf("  ⚠️  %d records with item_id=0\n", nullTorrents)
	} else {
		fmt.Println("  ✅ No zero item_ids")
	}

	var nullFileIDs int64
	db.QueryRow(`SELECT COUNT(*) FROM files WHERE file_id = 0`).Scan(&nullFileIDs)
	if nullFileIDs > 0 {
		fmt.Printf("  ⚠️  %d records with file_id=0\n", nullFileIDs)
	} else {
		fmt.Println("  ✅ No zero file_ids")
	}

	var nullSizes int64
	db.QueryRow(`SELECT COUNT(*) FROM files WHERE size = 0`).Scan(&nullSizes)
	if nullSizes > 0 {
		fmt.Printf("  ⚠️  %d records with size=0\n", nullSizes)
	} else {
		fmt.Println("  ✅ No zero-size files")
	}

	// (source, item_id, file_id) should always be unique in v2 schema.
	var dupKeys int64
	db.QueryRow(`SELECT COUNT(*) FROM (SELECT source, item_id, file_id FROM files GROUP BY source, item_id, file_id HAVING COUNT(*) > 1)`).Scan(&dupKeys)
	if dupKeys > 0 {
		fmt.Printf("  ⚠️  %d records with duplicate (source, item_id, file_id) — should not happen\n", dupKeys)
	} else {
		fmt.Println("  ✅ No duplicate (source, item_id, file_id) keys")
	}
}

func formatSize(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(bytes)/(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(bytes)/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.2f KB", float64(bytes)/(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}