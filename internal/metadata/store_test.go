package metadata

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := t.TempDir() + "/test.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	return s
}

func TestOpenAndClose(t *testing.T) {
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Errorf("Close failed: %v", err)
	}
}

func TestUpsertFile(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	f := FileRecord{
		ItemID:   100,
		FileID:   1,
		Source:   SourceTorrent,
		Name:     "test.mkv",
		Path:     "/Movies/test.mkv",
		Size:     1024,
		MimeType: "video/x-matroska",
	}
	if err := s.UpsertFile(f); err != nil {
		t.Fatalf("UpsertFile failed: %v", err)
	}
}

func TestGetFileByFileID(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	f := FileRecord{
		ItemID:   42,
		FileID:   1,
		Source:   SourceTorrent,
		Name:     "movie.mkv",
		Path:     "/Movies/movie.mkv",
		Size:     4096,
		MimeType: "video/x-matroska",
	}
	s.UpsertFile(f)

	got, err := s.GetFileByFileID(SourceTorrent, 1)
	if err != nil {
		t.Fatalf("GetFileByFileID failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected file, got nil")
	}
	if got.Name != "movie.mkv" {
		t.Errorf("name = %q, want %q", got.Name, "movie.mkv")
	}
	if got.Size != 4096 {
		t.Errorf("size = %d, want %d", got.Size, 4096)
	}
	if got.ItemID != 42 {
		t.Errorf("item_id = %d, want 42", got.ItemID)
	}
}

func TestGetFileByFileIDNotFound(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	got, err := s.GetFileByFileID(SourceTorrent, 999)
	if err != nil {
		t.Fatalf("GetFileByFileID failed: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing file_id, got %+v", got)
	}
}

func TestGetFileByPath(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	s.UpsertFile(FileRecord{
		ItemID:   1,
		FileID:   10,
		Source:   SourceTorrent,
		Name:     "file.txt",
		Path:     "/docs/file.txt",
		Size:     100,
	})

	got, err := s.GetFileByPath("/docs/file.txt")
	if err != nil {
		t.Fatalf("GetFileByPath failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected file, got nil")
	}
	if got.FileID != 10 {
		t.Errorf("file_id = %d, want 10", got.FileID)
	}
	if got.ItemID != 1 {
		t.Errorf("item_id = %d, want 1", got.ItemID)
	}
}

func TestListDir(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	files := []FileRecord{
		{ItemID: 1, FileID: 1, Source: SourceTorrent, Name: "a.mkv", Path: "/Movies/a.mkv", Size: 100},
		{ItemID: 1, FileID: 2, Source: SourceTorrent, Name: "b.mkv", Path: "/Movies/b.mkv", Size: 200},
		{ItemID: 2, FileID: 1, Source: SourceUsenet, Name: "c.mp3", Path: "/Music/c.mp3", Size: 300},
	}
	for _, f := range files {
		s.UpsertFile(f)
	}

	// List /Movies prefix.
	got, err := s.ListDir("/Movies")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 files in /Movies, got %d", len(got))
	}

	// List / (all files).
	got, err = s.ListDir("")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 files in /, got %d", len(got))
	}
}

func TestSetGetCDNURL(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	s.UpsertFile(FileRecord{ItemID: 1, FileID: 1, Source: SourceTorrent, Name: "f.mkv", Path: "/f.mkv", Size: 100})

	// Fetch the internal ID that was assigned.
	file, _ := s.GetFileByFileID(SourceTorrent, 1)
	internalID := file.ID

	// Set CDN URL with 1 hour expiry.
	expiry := time.Now().Add(1 * time.Hour)
	if err := s.SetCDNURL(internalID, "https://cdn.example.com/file", expiry); err != nil {
		t.Fatalf("SetCDNURL failed: %v", err)
	}

	// Get it back (should be fresh).
	url, err := s.GetCDNURL(internalID)
	if err != nil {
		t.Fatalf("GetCDNURL failed: %v", err)
	}
	if url != "https://cdn.example.com/file" {
		t.Errorf("got %q, want %q", url, "https://cdn.example.com/file")
	}
}

func TestGetExpiredCDNURL(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	s.UpsertFile(FileRecord{ItemID: 2, FileID: 1, Source: SourceTorrent, Name: "g.mkv", Path: "/g.mkv", Size: 100})
	file, _ := s.GetFileByFileID(SourceTorrent, 1)
	internalID := file.ID

	// Set CDN URL that already expired.
	expiry := time.Now().Add(-1 * time.Hour)
	if err := s.SetCDNURL(internalID, "https://cdn.example.com/old", expiry); err != nil {
		t.Fatalf("SetCDNURL failed: %v", err)
	}

	url, err := s.GetCDNURL(internalID)
	if err != nil {
		t.Fatalf("GetCDNURL failed: %v", err)
	}
	if url != "" {
		t.Errorf("expected empty for expired URL, got %q", url)
	}
}

func TestUpsertUpdatesExisting(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	s.UpsertFile(FileRecord{ItemID: 1, FileID: 1, Source: SourceTorrent, Name: "old.mkv", Path: "/same/path.mkv", Size: 100})
	s.UpsertFile(FileRecord{ItemID: 1, FileID: 1, Source: SourceTorrent, Name: "new.mkv", Path: "/same/path.mkv", Size: 200})

	got, _ := s.GetFileByFileID(SourceTorrent, 1)
	if got.Name != "new.mkv" {
		t.Errorf("name = %q, want %q", got.Name, "new.mkv")
	}
	if got.Size != 200 {
		t.Errorf("size = %d, want %d", got.Size, 200)
	}
}

func TestGetItemIDByFileID(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	s.UpsertFile(FileRecord{ItemID: 55, FileID: 7, Source: SourceTorrent, Name: "f.mkv", Path: "/f.mkv", Size: 100})

	tid, err := s.GetItemIDByFileID(SourceTorrent, 7)
	if err != nil {
		t.Fatalf("GetItemIDByFileID failed: %v", err)
	}
	if tid != 55 {
		t.Errorf("item_id = %d, want 55", tid)
	}
}

func TestDatabaseFileCreated(t *testing.T) {
	path := t.TempDir() + "/persist.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	s.Close()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("database file was not created on disk")
	}
}

func TestUpsertDuplicatePathDifferentTorrent(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	// Different torrents, same path — both rows are preserved because the
	// UNIQUE constraint is on (source, item_id, file_id), not path.
	s.UpsertFile(FileRecord{ItemID: 1, FileID: 1, Source: SourceTorrent, Name: "f.mkv", Path: "/f.mkv", Size: 100})
	s.UpsertFile(FileRecord{ItemID: 2, FileID: 1, Source: SourceTorrent, Name: "f.mkv", Path: "/f.mkv", Size: 200})

	// Both rows should exist.
	got, _ := s.GetFileByFileID(SourceTorrent, 1)
	if got == nil {
		t.Fatal("expected a file record, got nil")
	}
	// GetFileByFileID returns the first match; item_id could be 1 or 2.
	// The important thing is both records exist.

	// GetFileByPath returns the primary (highest id) — should be the second upsert.
	primary, err := s.GetFileByPath("/f.mkv")
	if err != nil {
		t.Fatalf("GetFileByPath failed: %v", err)
	}
	if primary.ItemID != 2 {
		t.Errorf("primary item_id = %d, want 2 (highest id wins)", primary.ItemID)
	}
	if primary.Size != 200 {
		t.Errorf("primary size = %d, want 200", primary.Size)
	}

	// GetFileAlternatives should return the other one.
	alternatives, err := s.GetFileAlternatives("/f.mkv")
	if err != nil {
		t.Fatalf("GetFileAlternatives failed: %v", err)
	}
	if len(alternatives) != 1 {
		t.Fatalf("expected 1 alternative, got %d", len(alternatives))
	}
	if alternatives[0].ItemID != 1 {
		t.Errorf("alternative item_id = %d, want 1", alternatives[0].ItemID)
	}
}

func TestGetFileByFileIDSourceDisambiguation(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	// Two files with the same file_id but different source: should coexist.
	s.UpsertFile(FileRecord{ItemID: 100, FileID: 1, Source: SourceTorrent, Name: "torrent.mkv", Path: "/torrent/file.mkv", Size: 500})
	s.UpsertFile(FileRecord{ItemID: 200, FileID: 1, Source: SourceUsenet, Name: "usenet.mkv", Path: "/usenet/file.mkv", Size: 300})

	// Look up by source=torrent should return the torrent file.
	torFile, err := s.GetFileByFileID(SourceTorrent, 1)
	if err != nil {
		t.Fatalf("GetFileByFileID(SourceTorrent, 1) failed: %v", err)
	}
	if torFile == nil {
		t.Fatal("expected torrent file, got nil")
	}
	if torFile.ItemID != 100 {
		t.Errorf("torrent item_id = %d, want 100", torFile.ItemID)
	}

	// Look up by source=usenet should return the usenet file.
	usenetFile, err := s.GetFileByFileID(SourceUsenet, 1)
	if err != nil {
		t.Fatalf("GetFileByFileID(SourceUsenet, 1) failed: %v", err)
	}
	if usenetFile == nil {
		t.Fatal("expected usenet file, got nil")
	}
	if usenetFile.ItemID != 200 {
		t.Errorf("usenet item_id = %d, want 200", usenetFile.ItemID)
	}

	// Verify they are different records.
	if torFile.ID == usenetFile.ID {
		t.Error("torrent and usenet files should have different internal IDs")
	}
}

func TestGetItemIDByFileIDSourceDisambiguation(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	s.UpsertFile(FileRecord{ItemID: 100, FileID: 5, Source: SourceTorrent, Name: "t.mkv", Path: "/t.mkv", Size: 100})
	s.UpsertFile(FileRecord{ItemID: 200, FileID: 5, Source: SourceUsenet, Name: "u.mkv", Path: "/u.mkv", Size: 200})

	// Should return the correct item_id for each source.
	torID, err := s.GetItemIDByFileID(SourceTorrent, 5)
	if err != nil {
		t.Fatalf("GetItemIDByFileID(SourceTorrent, 5) failed: %v", err)
	}
	if torID != 100 {
		t.Errorf("torrent item_id = %d, want 100", torID)
	}

	usenetID, err := s.GetItemIDByFileID(SourceUsenet, 5)
	if err != nil {
		t.Fatalf("GetItemIDByFileID(SourceUsenet, 5) failed: %v", err)
	}
	if usenetID != 200 {
		t.Errorf("usenet item_id = %d, want 200", usenetID)
	}
}

func TestGetNextSyncTag_firstCall(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	tag, err := s.GetNextSyncTag()
	if err != nil {
		t.Fatalf("GetNextSyncTag failed: %v", err)
	}
	if tag != 1 {
		t.Errorf("first call: expected tag 1, got %d", tag)
	}
}

func TestGetNextSyncTag_increments(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	for i := int64(1); i <= 5; i++ {
		tag, err := s.GetNextSyncTag()
		if err != nil {
			t.Fatalf("iteration %d: GetNextSyncTag failed: %v", i, err)
		}
		if tag != i {
			t.Errorf("iteration %d: expected tag %d, got %d", i, i, tag)
		}
	}
}

func TestGetNextSyncTag_persistsAcrossOpen(t *testing.T) {
	path := t.TempDir() + "/test_sync_tag.db"
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open failed: %v", err)
	}
	tag1, err := s1.GetNextSyncTag()
	if err != nil {
		t.Fatalf("first GetNextSyncTag failed: %v", err)
	}
	s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open failed: %v", err)
	}
	defer s2.Close()

	tag2, err := s2.GetNextSyncTag()
	if err != nil {
		t.Fatalf("second GetNextSyncTag failed: %v", err)
	}
	if tag2 != tag1+1 {
		t.Errorf("expected tag %d (prev + 1), got %d", tag1+1, tag2)
	}
}

func TestListItemDirs_singleFile(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	if err := s.UpsertFile(FileRecord{
		ItemID: 1, FileID: 10, Source: SourceTorrent,
		Name: "movie.mkv", Path: "movie.mkv",
		Size: 1000, MimeType: "video/x-matroska",
	}); err != nil {
		t.Fatalf("UpsertFile failed: %v", err)
	}

	dirs, err := s.ListItemDirs()
	if err != nil {
		t.Fatalf("ListItemDirs failed: %v", err)
	}

	if len(dirs) != 1 {
		t.Fatalf("expected 1 item dir, got %d", len(dirs))
	}
	if dirs[0].ItemID != 1 {
		t.Errorf("item_id = %d, want 1", dirs[0].ItemID)
	}
	if dirs[0].Source != SourceTorrent {
		t.Errorf("source = %d, want SourceTorrent (%d)", dirs[0].Source, SourceTorrent)
	}
	if dirs[0].Dir != "movie.mkv" {
		t.Errorf("dir = %q, want %q", dirs[0].Dir, "movie.mkv")
	}
}

func TestListItemDirs_multiFileTorrent(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	if err := s.UpsertFile(FileRecord{
		ItemID: 1, FileID: 10, Source: SourceTorrent,
		Name: "file1.mkv", Path: "Season 1/file1.mkv",
		Size: 1000, MimeType: "video/x-matroska",
	}); err != nil {
		t.Fatalf("UpsertFile failed: %v", err)
	}
	if err := s.UpsertFile(FileRecord{
		ItemID: 1, FileID: 11, Source: SourceTorrent,
		Name: "file2.mkv", Path: "Season 1/file2.mkv",
		Size: 2000, MimeType: "video/x-matroska",
	}); err != nil {
		t.Fatalf("UpsertFile failed: %v", err)
	}

	dirs, err := s.ListItemDirs()
	if err != nil {
		t.Fatalf("ListItemDirs failed: %v", err)
	}

	if len(dirs) != 1 {
		t.Fatalf("expected 1 distinct dir, got %d", len(dirs))
	}
	if dirs[0].Dir != "Season 1" {
		t.Errorf("dir = %q, want %q", dirs[0].Dir, "Season 1")
	}
}

func TestListItemDirs_twoTorrents(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	if err := s.UpsertFile(FileRecord{
		ItemID: 1, FileID: 10, Source: SourceTorrent,
		Name: "movie1.mkv", Path: "movie1.mkv",
		Size: 1000, MimeType: "video/x-matroska",
	}); err != nil {
		t.Fatalf("UpsertFile failed: %v", err)
	}
	if err := s.UpsertFile(FileRecord{
		ItemID: 2, FileID: 20, Source: SourceTorrent,
		Name: "movie2.mkv", Path: "TV/movie2.mkv",
		Size: 2000, MimeType: "video/x-matroska",
	}); err != nil {
		t.Fatalf("UpsertFile failed: %v", err)
	}

	dirs, err := s.ListItemDirs()
	if err != nil {
		t.Fatalf("ListItemDirs failed: %v", err)
	}

	if len(dirs) != 2 {
		t.Fatalf("expected 2 item dirs, got %d", len(dirs))
	}

	dirMap := make(map[int64]string)
	for _, d := range dirs {
		dirMap[d.ItemID] = d.Dir
	}
	if dirMap[1] != "movie1.mkv" {
		t.Errorf("item 1: expected dir %q, got %q", "movie1.mkv", dirMap[1])
	}
	if dirMap[2] != "TV" {
		t.Errorf("item 2: expected dir %q, got %q", "TV", dirMap[2])
	}
}

func TestListItemDirs_subSubDirectory(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	if err := s.UpsertFile(FileRecord{
		ItemID: 1, FileID: 10, Source: SourceTorrent,
		Name: "file.mkv", Path: "Season 1/Episodes/file.mkv",
		Size: 1000, MimeType: "video/x-matroska",
	}); err != nil {
		t.Fatalf("UpsertFile failed: %v", err)
	}

	dirs, err := s.ListItemDirs()
	if err != nil {
		t.Fatalf("ListItemDirs failed: %v", err)
	}

	if len(dirs) != 1 {
		t.Fatalf("expected 1 item dir, got %d", len(dirs))
	}
	if dirs[0].Dir != "Season 1" {
		t.Errorf("dir = %q, want first segment %q", dirs[0].Dir, "Season 1")
	}
}

func TestPruneBySyncTag_removesNonMatching(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	if err := s.UpsertFile(FileRecord{
		ItemID: 1, FileID: 10, Source: SourceTorrent,
		Name: "keep.mkv", Path: "keep.mkv", Size: 100,
		SyncTag: 2,
	}); err != nil {
		t.Fatalf("UpsertFile keep failed: %v", err)
	}
	if err := s.UpsertFile(FileRecord{
		ItemID: 2, FileID: 20, Source: SourceTorrent,
		Name: "stale.mkv", Path: "stale.mkv", Size: 200,
		SyncTag: 1,
	}); err != nil {
		t.Fatalf("UpsertFile stale failed: %v", err)
	}
	if err := s.UpsertFile(FileRecord{
		ItemID: 3, FileID: 30, Source: SourceTorrent,
		Name: "unsynced.mkv", Path: "unsynced.mkv", Size: 300,
		SyncTag: 0,
	}); err != nil {
		t.Fatalf("UpsertFile unsynced failed: %v", err)
	}

	n, err := s.PruneBySyncTag(2)
	if err != nil {
		t.Fatalf("PruneBySyncTag failed: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 rows pruned, got %d", n)
	}

	keep, err := s.GetFileByPath("keep.mkv")
	if err != nil {
		t.Fatalf("GetFileByPath failed: %v", err)
	}
	if keep == nil {
		t.Error("file with matching sync_tag should survive")
	}

	if stale, _ := s.GetFileByPath("stale.mkv"); stale != nil {
		t.Error("file with non-matching sync_tag should have been removed")
	}
	if unsynced, _ := s.GetFileByPath("unsynced.mkv"); unsynced != nil {
		t.Error("file with sync_tag=0 should have been removed")
	}
}

func TestPruneBySyncTag_emptyStore(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	n, err := s.PruneBySyncTag(1)
	if err != nil {
		t.Fatalf("PruneBySyncTag failed: %v", err)
	}
	if n != 0 {
		t.Errorf("empty store: expected 0 rows pruned, got %d", n)
	}
}

func TestPruneBySyncTag_invalidTag(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	_, err := s.PruneBySyncTag(0)
	if err == nil {
		t.Error("expected error for tag 0")
	}
	_, err = s.PruneBySyncTag(-1)
	if err == nil {
		t.Error("expected error for tag -1")
	}
}

func TestPruneBySyncTag_batchMultiple(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	for i := int64(0); i < 300; i++ {
		if err := s.UpsertFile(FileRecord{
			ItemID: i, FileID: i, Source: SourceTorrent,
			Name: fmt.Sprintf("file%d.mkv", i),
			Path: fmt.Sprintf("file%d.mkv", i),
			Size: 100, SyncTag: 1,
		}); err != nil {
			t.Fatalf("UpsertFile %d failed: %v", i, err)
		}
	}

	if err := s.UpsertFile(FileRecord{
		ItemID: 999, FileID: 999, Source: SourceTorrent,
		Name: "survivor.mkv", Path: "survivor.mkv",
		Size: 100, SyncTag: 2,
	}); err != nil {
		t.Fatalf("UpsertFile survivor failed: %v", err)
	}

	n, err := s.PruneBySyncTag(2)
	if err != nil {
		t.Fatalf("PruneBySyncTag failed: %v", err)
	}
	if n != 300 {
		t.Errorf("expected 300 rows pruned, got %d", n)
	}

	survivor, err := s.GetFileByPath("survivor.mkv")
	if err != nil {
		t.Fatalf("GetFileByPath failed: %v", err)
	}
	if survivor == nil {
		t.Error("survivor file with sync_tag=2 should survive")
	}
}

func TestGetFileAlternatives(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	// Two different items with the same path.
	s.UpsertFile(FileRecord{ItemID: 10, FileID: 1, Source: SourceTorrent, Name: "f.mkv", Path: "/dup.mkv", Size: 100})
	s.UpsertFile(FileRecord{ItemID: 20, FileID: 1, Source: SourceTorrent, Name: "f.mkv", Path: "/dup.mkv", Size: 200})
	// Third item, different path — should not appear in alternatives for /dup.mkv.
	s.UpsertFile(FileRecord{ItemID: 30, FileID: 1, Source: SourceTorrent, Name: "other.mkv", Path: "/other.mkv", Size: 300})

	alternatives, err := s.GetFileAlternatives("/dup.mkv")
	if err != nil {
		t.Fatalf("GetFileAlternatives failed: %v", err)
	}
	if len(alternatives) != 1 {
		t.Fatalf("expected 1 alternative, got %d", len(alternatives))
	}
	// The alternative should be the first-upserted item (lower internal id).
	if alternatives[0].ItemID != 10 {
		t.Errorf("alternative item_id = %d, want 10", alternatives[0].ItemID)
	}
}

func TestGetFileAlternativesNoDuplicates(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	s.UpsertFile(FileRecord{ItemID: 1, FileID: 1, Source: SourceTorrent, Name: "unique.mkv", Path: "/unique.mkv", Size: 100})

	alternatives, err := s.GetFileAlternatives("/unique.mkv")
	if err != nil {
		t.Fatalf("GetFileAlternatives failed: %v", err)
	}
	if len(alternatives) != 0 {
		t.Fatalf("expected 0 alternatives for unique path, got %d", len(alternatives))
	}
}

func TestCountDistinctPaths(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	// Two items with the same path.
	s.UpsertFile(FileRecord{ItemID: 1, FileID: 1, Source: SourceTorrent, Name: "a.mkv", Path: "/dup.mkv", Size: 100})
	s.UpsertFile(FileRecord{ItemID: 2, FileID: 1, Source: SourceTorrent, Name: "a.mkv", Path: "/dup.mkv", Size: 200})
	// Different path.
	s.UpsertFile(FileRecord{ItemID: 3, FileID: 1, Source: SourceTorrent, Name: "b.mkv", Path: "/unique.mkv", Size: 300})

	total, err := s.CountFiles()
	if err != nil {
		t.Fatalf("CountFiles failed: %v", err)
	}
	if total != 3 {
		t.Errorf("CountFiles = %d, want 3", total)
	}

	distinct, err := s.CountDistinctPaths()
	if err != nil {
		t.Fatalf("CountDistinctPaths failed: %v", err)
	}
	if distinct != 2 {
		t.Errorf("CountDistinctPaths = %d, want 2", distinct)
	}
}

func TestListDirDedup(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	// Two items with the same file path.
	s.UpsertFile(FileRecord{ItemID: 1, FileID: 1, Source: SourceTorrent, Name: "f.mkv", Path: "/dir/f.mkv", Size: 100})
	s.UpsertFile(FileRecord{ItemID: 2, FileID: 1, Source: SourceTorrent, Name: "f.mkv", Path: "/dir/f.mkv", Size: 200})
	// A different file in the same dir.
	s.UpsertFile(FileRecord{ItemID: 3, FileID: 1, Source: SourceTorrent, Name: "g.mkv", Path: "/dir/g.mkv", Size: 300})

	files, err := s.ListDir("/dir/")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files in /dir/ (deduped), got %d", len(files))
	}

	// The primary (highest id) for f.mkv should be item_id=2.
	for _, f := range files {
		if f.Name == "f.mkv" && f.ItemID != 2 {
			t.Errorf("f.mkv item_id = %d, want 2 (primary should be highest id)", f.ItemID)
		}
	}
}

func TestGetFileByPathReturnsPrimary(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	// Two items, same path. Primary should be the second (highest id).
	s.UpsertFile(FileRecord{ItemID: 1, FileID: 1, Source: SourceTorrent, Name: "f.mkv", Path: "/f.mkv", Size: 100})
	s.UpsertFile(FileRecord{ItemID: 2, FileID: 1, Source: SourceTorrent, Name: "f.mkv", Path: "/f.mkv", Size: 200})

	f, err := s.GetFileByPath("/f.mkv")
	if err != nil {
		t.Fatalf("GetFileByPath failed: %v", err)
	}
	if f == nil {
		t.Fatal("GetFileByPath returned nil")
	}
	if f.ItemID != 2 {
		t.Errorf("primary item_id = %d, want 2", f.ItemID)
	}
}

func TestMigrateAutoRecreatesV1DB(t *testing.T) {
	// Create a v1-schema database file and verify Open() auto-recreates it to v2.
	path := t.TempDir() + "/test_v1.db"

	v1Schema := `
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
	`
	rawDB, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		t.Fatalf("opening raw v1 db: %v", err)
	}
	if _, err := rawDB.Exec(v1Schema); err != nil {
		t.Fatalf("creating v1 schema: %v", err)
	}
	// Verify v1 schema was created correctly.
	var createSQL string
	if err := rawDB.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'files'`).Scan(&createSQL); err != nil {
		t.Fatalf("reading v1 schema: %v", err)
	}
	if !strings.Contains(createSQL, "UNIQUE") {
		t.Fatal("v1 db should have a UNIQUE constraint")
	}
	rawDB.Close()

	// Now open with the new code — migrate() should detect v1 and auto-recreate.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on v1 db failed (should auto-recreate): %v", err)
	}
	defer s.Close()

	// Verify the DB was recreated with v2 schema.
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'files'`).Scan(&createSQL); err != nil {
		t.Fatalf("reading v2 schema: %v", err)
	}
	if !strings.Contains(createSQL, "UNIQUE(source, item_id, file_id)") {
		t.Fatal("recreated db should have v2 unique constraint")
	}

	// Verify PRAGMA user_version was set.
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if version != 2 {
		t.Errorf("user_version = %d, want 2", version)
	}

	// Verify upserts work with the new schema.
	if err := s.UpsertFile(FileRecord{ItemID: 1, FileID: 1, Source: SourceTorrent, Name: "test.mkv", Path: "/test.mkv", Size: 100}); err != nil {
		t.Fatalf("UpsertFile on migrated db failed: %v", err)
	}
	f, err := s.GetFileByPath("/test.mkv")
	if err != nil || f == nil || f.ItemID != 1 {
		t.Fatal("migrated db should allow upserts and lookups")
	}
}
