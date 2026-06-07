package metadata

import (
	"os"
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
		ID:       100,
		Name:     "test.mkv",
		Path:     "/Movies/test.mkv",
		Size:     1024,
		MimeType: "video/x-matroska",
	}
	if err := s.UpsertFile(f); err != nil {
		t.Fatalf("UpsertFile failed: %v", err)
	}
}

func TestGetFileByID(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	f := FileRecord{
		ID:       42,
		Name:     "movie.mkv",
		Path:     "/Movies/movie.mkv",
		Size:     4096,
		MimeType: "video/x-matroska",
	}
	s.UpsertFile(f)

	got, err := s.GetFileByID(42)
	if err != nil {
		t.Fatalf("GetFileByID failed: %v", err)
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
}

func TestGetFileByIDNotFound(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	got, err := s.GetFileByID(999)
	if err != nil {
		t.Fatalf("GetFileByID failed: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing ID, got %+v", got)
	}
}

func TestGetFileByPath(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	s.UpsertFile(FileRecord{
		ID:   1,
		Name: "file.txt",
		Path: "/docs/file.txt",
		Size: 100,
	})

	got, err := s.GetFileByPath("/docs/file.txt")
	if err != nil {
		t.Fatalf("GetFileByPath failed: %v", err)
	}
	if got == nil {
		t.Fatal("expected file, got nil")
	}
	if got.ID != 1 {
		t.Errorf("id = %d, want 1", got.ID)
	}
}

func TestListDir(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	files := []FileRecord{
		{ID: 1, Name: "a.mkv", Path: "/Movies/a.mkv", Size: 100},
		{ID: 2, Name: "b.mkv", Path: "/Movies/b.mkv", Size: 200},
		{ID: 3, Name: "c.mp3", Path: "/Music/c.mp3", Size: 300},
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

	s.UpsertFile(FileRecord{ID: 1, Name: "f.mkv", Path: "/f.mkv", Size: 100})

	// Set CDN URL with 1 hour expiry.
	expiry := time.Now().Add(1 * time.Hour)
	if err := s.SetCDNURL(1, "https://cdn.example.com/file", expiry); err != nil {
		t.Fatalf("SetCDNURL failed: %v", err)
	}

	// Get it back (should be fresh).
	url, err := s.GetCDNURL(1)
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

	s.UpsertFile(FileRecord{ID: 2, Name: "g.mkv", Path: "/g.mkv", Size: 100})

	// Set CDN URL that already expired.
	expiry := time.Now().Add(-1 * time.Hour)
	if err := s.SetCDNURL(2, "https://cdn.example.com/old", expiry); err != nil {
		t.Fatalf("SetCDNURL failed: %v", err)
	}

	url, err := s.GetCDNURL(2)
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

	s.UpsertFile(FileRecord{ID: 1, Name: "old.mkv", Path: "/same/path.mkv", Size: 100})
	s.UpsertFile(FileRecord{ID: 1, Name: "new.mkv", Path: "/same/path.mkv", Size: 200})

	got, _ := s.GetFileByID(1)
	if got.Name != "new.mkv" {
		t.Errorf("name = %q, want %q", got.Name, "new.mkv")
	}
	if got.Size != 200 {
		t.Errorf("size = %d, want %d", got.Size, 200)
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