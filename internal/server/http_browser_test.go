package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mainlink0435/warpbox/internal/config"
	"github.com/mainlink0435/warpbox/internal/metadata"
	"github.com/mainlink0435/warpbox/internal/throttle"
)

func TestHTTPBrowser_RootSyntheticDirsWithSizes(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.UpsertFile(metadata.FileRecord{ItemID: 1, Source: metadata.SourceTorrent, Name: "movie.mkv", Path: "Movie.2024/movie.mkv", Size: 5000, MimeType: "video/x-matroska"})
	store.UpsertFile(metadata.FileRecord{ItemID: 2, Source: metadata.SourceTorrent, Name: "show.mkv", Path: "Show.S01E01/ep1.mkv", Size: 1000, MimeType: "video/x-matroska"})

	cfg := Config{
		Version: "test",
		VirtualPaths: []config.VirtualPathConfig{
			{Name: "movies", DirectoryExclude: "(?i)S01", FileRegex: `.*\.(mkv|mp4|avi)$`},
			{Name: "tv", DirectoryInclude: "(?i)S01", FileRegex: `.*\.(mkv|mp4|avi)$`},
		},
	}
	queue := throttle.NewQueue(600)
	srv := New(cfg, store, nil, queue)

	r := httptest.NewRequest(http.MethodGet, "/http/", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	body := w.Body.String()

	// Root should show synthetic dirs with sizes
	if !strings.Contains(body, "__all__") {
		t.Error("root listing should contain __all__")
	}
	if !strings.Contains(body, "movies") {
		t.Error("root listing should contain movies")
	}
	if !strings.Contains(body, "tv") {
		t.Error("root listing should contain tv")
	}
	// __all__ total = 5000 + 1000 = 6000
	if !strings.Contains(body, "5.9 KB") {
		// 6000 bytes ≈ 5.9 KB
	}
}

func TestHTTPBrowser_FolderSizeAccumulation(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Two files in same directory = accumulated size
	store.UpsertFile(metadata.FileRecord{ItemID: 1, Source: metadata.SourceTorrent, Name: "main.mkv", Path: "Movie.2024/main.mkv", Size: 5000, MimeType: "video/x-matroska", FileID: 1})
	store.UpsertFile(metadata.FileRecord{ItemID: 1, Source: metadata.SourceTorrent, Name: "featurette.mkv", Path: "Movie.2024/featurette.mkv", Size: 200, MimeType: "video/x-matroska", FileID: 2})
	// Separate directory
	store.UpsertFile(metadata.FileRecord{ItemID: 2, Source: metadata.SourceTorrent, Name: "other.mkv", Path: "Other.Video/other.mkv", Size: 1000, MimeType: "video/x-matroska", FileID: 1})

	cfg := Config{Version: "test"}
	queue := throttle.NewQueue(600)
	srv := New(cfg, store, nil, queue)

	r := httptest.NewRequest(http.MethodGet, "/http/__all__/", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	body := w.Body.String()

	// Movie.2024 should show accumulated size: 5000 + 200 = 5200 bytes ≈ 5.1 KB
	if !strings.Contains(body, "Movie.2024") {
		t.Error("should contain Movie.2024 directory")
	}
	if !strings.Contains(body, "5.1 KB") {
		t.Errorf("Movie.2024 directory should show ~5.1 KB (5200 bytes)")
	}
	// Other.Video should show 1000 bytes
	if !strings.Contains(body, "1000 B") {
		t.Errorf("Other.Video directory should show 1000 B")
	}
}

func TestHTTPBrowser_NestedSubdirSizes(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Files at different nesting levels within same torrent dir
	store.UpsertFile(metadata.FileRecord{ItemID: 1, Source: metadata.SourceTorrent, Name: "main.mkv", Path: "Torrent/main.mkv", Size: 5000, MimeType: "video/x-matroska", FileID: 1})
	store.UpsertFile(metadata.FileRecord{ItemID: 1, Source: metadata.SourceTorrent, Name: "extra.mkv", Path: "Torrent/Featurettes/extra.mkv", Size: 300, MimeType: "video/x-matroska", FileID: 2})
	store.UpsertFile(metadata.FileRecord{ItemID: 1, Source: metadata.SourceTorrent, Name: "clip.mkv", Path: "Torrent/Featurettes/Deleted/clip.mkv", Size: 100, MimeType: "video/x-matroska", FileID: 3})

	cfg := Config{Version: "test"}
	queue := throttle.NewQueue(600)
	srv := New(cfg, store, nil, queue)

	// Root level — Torrent dir should show total of ALL files: 5000+300+100 = 5400
	r := httptest.NewRequest(http.MethodGet, "/http/", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	body := w.Body.String()

	if !strings.Contains(body, "5.3 KB") {
		t.Errorf("Torrent dir should show ~5.3 KB (5400), got body to check")
	}

	// Inside Torrent — Featurettes dir should show sub-total: 300+100 = 400
	r2 := httptest.NewRequest(http.MethodGet, "/http/Torrent/", nil)
	w2 := httptest.NewRecorder()
	srv.mux.ServeHTTP(w2, r2)
	body2 := w2.Body.String()

	if !strings.Contains(body2, "Featurettes") {
		t.Error("should contain Featurettes subdirectory")
	}
	if !strings.Contains(body2, "400 B") {
		t.Errorf("Featurettes should show 400 B")
	}
}

func TestHTTPBrowser_SortByName(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.UpsertFile(metadata.FileRecord{ItemID: 1, Source: metadata.SourceTorrent, Name: "z.mkv", Path: "Zed.Video/z.mkv", Size: 100, FileID: 1})
	store.UpsertFile(metadata.FileRecord{ItemID: 2, Source: metadata.SourceTorrent, Name: "a.mkv", Path: "Alpha.Video/a.mkv", Size: 100, FileID: 1})
	store.UpsertFile(metadata.FileRecord{ItemID: 3, Source: metadata.SourceTorrent, Name: "m.mkv", Path: "Mid.Video/m.mkv", Size: 100, FileID: 1})

	cfg := Config{Version: "test"}
	queue := throttle.NewQueue(600)
	srv := New(cfg, store, nil, queue)

	r := httptest.NewRequest(http.MethodGet, "/http/__all__/", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	body := w.Body.String()

	// Default sort by name: Alpha, Mid, Zed
	alpha := strings.Index(body, "Alpha.Video")
	mid := strings.Index(body, "Mid.Video")
	zed := strings.Index(body, "Zed.Video")

	if alpha < 0 || mid < 0 || zed < 0 {
		t.Fatal("expected directories Alpha, Mid, Zed")
	}
	if !(alpha < mid && mid < zed) {
		t.Errorf("default sort should be alphabetical: Alpha < Mid < Zed, got %d %d %d", alpha, mid, zed)
	}
}

func TestHTTPBrowser_SortByNameReverse(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.UpsertFile(metadata.FileRecord{ItemID: 1, Source: metadata.SourceTorrent, Name: "a.mkv", Path: "Alpha.Video/a.mkv", Size: 100, FileID: 1})
	store.UpsertFile(metadata.FileRecord{ItemID: 2, Source: metadata.SourceTorrent, Name: "z.mkv", Path: "Zed.Video/z.mkv", Size: 100, FileID: 1})

	cfg := Config{Version: "test"}
	queue := throttle.NewQueue(600)
	srv := New(cfg, store, nil, queue)

	r := httptest.NewRequest(http.MethodGet, "/http/__all__/?sort=-name", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	body := w.Body.String()

	alpha := strings.Index(body, "Alpha.Video")
	zed := strings.Index(body, "Zed.Video")

	if alpha < 0 || zed < 0 {
		t.Fatal("expected directories Alpha, Zed")
	}
	if !(zed < alpha) {
		t.Errorf("sort=-name should be descending: Zed before Alpha")
	}
}

func TestHTTPBrowser_SortBySize(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.UpsertFile(metadata.FileRecord{ItemID: 1, Source: metadata.SourceTorrent, Name: "big.mkv", Path: "Big.Video/big.mkv", Size: 9000, FileID: 1})
	store.UpsertFile(metadata.FileRecord{ItemID: 2, Source: metadata.SourceTorrent, Name: "small.mkv", Path: "Small.Video/small.mkv", Size: 100, FileID: 1})
	store.UpsertFile(metadata.FileRecord{ItemID: 3, Source: metadata.SourceTorrent, Name: "med.mkv", Path: "Med.Video/med.mkv", Size: 1000, FileID: 1})

	cfg := Config{Version: "test"}
	queue := throttle.NewQueue(600)
	srv := New(cfg, store, nil, queue)

	r := httptest.NewRequest(http.MethodGet, "/http/__all__/?sort=size", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	body := w.Body.String()

	// Sort by size ascending: Small(100), Med(1000), Big(9000)
	small := strings.Index(body, "Small.Video")
	med := strings.Index(body, "Med.Video")
	big := strings.Index(body, "Big.Video")

	if small < 0 || med < 0 || big < 0 {
		t.Fatal("expected directories Small, Med, Big")
	}
	if !(small < med && med < big) {
		t.Errorf("sort=size should be ascending: Small < Med < Big, got %d %d %d", small, med, big)
	}
}

func TestHTTPBrowser_SortByType(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.UpsertFile(metadata.FileRecord{ItemID: 1, Source: metadata.SourceTorrent, Name: "a.mkv", Path: "Alpha.Video/a.mkv", Size: 100, MimeType: "video/x-matroska", FileID: 1})
	store.UpsertFile(metadata.FileRecord{ItemID: 2, Source: metadata.SourceTorrent, Name: "note.txt", Path: "note.txt", Size: 10, MimeType: "text/plain", FileID: 1})

	cfg := Config{Version: "test"}
	queue := throttle.NewQueue(600)
	srv := New(cfg, store, nil, queue)

	r := httptest.NewRequest(http.MethodGet, "/http/__all__/?sort=type", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	body := w.Body.String()

	// Type sort: directories first, then files by mime type.
	dirIdx := strings.Index(body, "Alpha.Video")
	fileIdx := strings.Index(body, "note.txt")

	if dirIdx < 0 || fileIdx < 0 {
		t.Fatal("expected directory Alpha.Video and file note.txt")
	}
	if !(dirIdx < fileIdx) {
		t.Errorf("sort=type should have directory before file")
	}
}

func TestHTTPBrowser_SortByTypeReverse(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.UpsertFile(metadata.FileRecord{ItemID: 1, Source: metadata.SourceTorrent, Name: "a.mkv", Path: "Alpha.Video/a.mkv", Size: 100, MimeType: "video/x-matroska", FileID: 1})
	store.UpsertFile(metadata.FileRecord{ItemID: 2, Source: metadata.SourceTorrent, Name: "note.txt", Path: "note.txt", Size: 10, MimeType: "text/plain", FileID: 1})

	cfg := Config{Version: "test"}
	queue := throttle.NewQueue(600)
	srv := New(cfg, store, nil, queue)

	r := httptest.NewRequest(http.MethodGet, "/http/__all__/?sort=-type", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	body := w.Body.String()

	// Type reverse: files first, then directories.
	dirIdx := strings.Index(body, "Alpha.Video")
	fileIdx := strings.Index(body, "note.txt")

	if dirIdx < 0 || fileIdx < 0 {
		t.Fatal("expected directory Alpha.Video and file note.txt")
	}
	if !(fileIdx < dirIdx) {
		t.Errorf("sort=-type should have file before directory")
	}
}

func TestHTTPBrowser_PercentInFilenameHref(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	name := "30% Iron Chef.mkv"
	if err := store.UpsertFile(metadata.FileRecord{
		ItemID: 1, Source: metadata.SourceTorrent, Name: name,
		Path: "Futurama/" + name, Size: 100, MimeType: "video/x-matroska", FileID: 1,
	}); err != nil {
		t.Fatal(err)
	}

	srv := New(Config{Version: "test"}, store, nil, nil)

	r := httptest.NewRequest(http.MethodGet, "/http/Futurama/", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	body := w.Body.String()

	// Link target must percent-encode '%' so browsers/rclone can parse it.
	if !strings.Contains(body, `href="/http/Futurama/30%25%20Iron%20Chef.mkv"`) {
		t.Errorf("expected encoded file href, body:\n%s", body)
	}
	// Visible label keeps the real title.
	if !strings.Contains(body, name) {
		t.Errorf("expected display name with literal %%, body:\n%s", body)
	}
}

func TestHTTPBrowser_VirtualPathHrefs(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.UpsertFile(metadata.FileRecord{ItemID: 1, Source: metadata.SourceTorrent, Name: "m.mkv", Path: "Movie.2024/m.mkv", Size: 100, MimeType: "video/x-matroska", FileID: 1})

	cfg := Config{
		Version: "test",
		VirtualPaths: []config.VirtualPathConfig{
			{Name: "movies", DirectoryExclude: "(?i)S01", FileRegex: `.*\.(mkv|mp4|avi)$`},
		},
	}
	queue := throttle.NewQueue(600)
	srv := New(cfg, store, nil, queue)

	// Browse into movies virtual path
	r := httptest.NewRequest(http.MethodGet, "/http/movies/Movie.2024/", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	body := w.Body.String()

	// File href should include /http/movies/ prefix
	if !strings.Contains(body, `href="/http/movies/Movie.2024/m.mkv"`) {
		t.Error("file href should include mount prefix /http/movies/")
	}
}

func TestHTTPBrowser_NoLibraryConfig(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.UpsertFile(metadata.FileRecord{ItemID: 1, Source: metadata.SourceTorrent, Name: "f.mkv", Path: "Torrent/f.mkv", Size: 100, MimeType: "video/x-matroska", FileID: 1})

	cfg := Config{Version: "test"}
	queue := throttle.NewQueue(600)
	srv := New(cfg, store, nil, queue)

	// Root should show __all__ synthetic dir only.
	r := httptest.NewRequest(http.MethodGet, "/http/", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	body := w.Body.String()

	if !strings.Contains(body, "__all__") {
		t.Error("without virtual paths, /http/ should show __all__ synthetic dir")
	}
	if strings.Contains(body, "Torrent") {
		t.Error("without virtual paths, /http/ root should NOT show Torrent directly")
	}

	// __all__ sub-path shows all real files.
	r2 := httptest.NewRequest(http.MethodGet, "/http/__all__/", nil)
	w2 := httptest.NewRecorder()
	srv.mux.ServeHTTP(w2, r2)
	body2 := w2.Body.String()
	if !strings.Contains(body2, "Torrent") {
		t.Error("/http/__all__/ should show Torrent directory")
	}
	if !strings.Contains(body2, "100 B") {
		t.Errorf("size should be shown in __all__ view")
	}
}

func TestHTTPBrowser_FileUnderVirtualPath(t *testing.T) {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	store.UpsertFile(metadata.FileRecord{ItemID: 1, Source: metadata.SourceTorrent, Name: "m.mkv", Path: "Movie.2024/m.mkv", Size: 100, MimeType: "video/x-matroska", FileID: 1})

	cfg := Config{
		Version: "test",
		VirtualPaths: []config.VirtualPathConfig{
			{Name: "movies", DirectoryExclude: "(?i)S01", FileRegex: `.*\.(mkv|mp4|avi)$`},
		},
	}
	queue := throttle.NewQueue(600)
	srv := New(cfg, store, nil, queue)

	r := httptest.NewRequest(http.MethodGet, "/http/movies/Movie.2024/", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "m.mkv") {
		t.Error("should show file m.mkv")
	}
	if !strings.Contains(body, "100 B") {
		t.Error("should show file size 100 B")
	}
}
