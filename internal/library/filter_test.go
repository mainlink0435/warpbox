package library

import (
	"testing"

	"github.com/mainlink0435/warpbox/internal/metadata"
)

func TestNewFilter(t *testing.T) {
	f, err := NewFilter("/tv", "(?i)season", "", `.*\.(mkv|mp4)$`, true)
	if err != nil {
		t.Fatalf("NewFilter failed: %v", err)
	}
	if f.Mount != "/tv" {
		t.Errorf("Mount = %q, want /tv", f.Mount)
	}
	if !f.LargestFileOnly {
		t.Error("LargestFileOnly should be true")
	}
}

func TestNewFilter_EmptyRegex(t *testing.T) {
	f, err := NewFilter("/all", "", "", "", false)
	if err != nil {
		t.Fatalf("NewFilter failed: %v", err)
	}
	if f.DirectoryInclude != nil {
		t.Error("DirectoryInclude should be nil for empty string")
	}
	if f.DirectoryExclude != nil {
		t.Error("DirectoryExclude should be nil for empty string")
	}
	if f.FileRegex != nil {
		t.Error("FileRegex should be nil for empty string")
	}
}

func TestNewFilter_InvalidInclude(t *testing.T) {
	_, err := NewFilter("/bad", "[invalid", "", "", false)
	if err == nil {
		t.Fatal("expected error for invalid include regex")
	}
}

func TestNewFilter_InvalidExclude(t *testing.T) {
	_, err := NewFilter("/bad", "", "[invalid", "", false)
	if err == nil {
		t.Fatal("expected error for invalid exclude regex")
	}
}

func TestNewFilter_InvalidFile(t *testing.T) {
	_, err := NewFilter("/bad", "", "", "[invalid", false)
	if err == nil {
		t.Fatal("expected error for invalid file regex")
	}
}

func TestExtractDirectory(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"Movie.Name.1999/file.mkv", "Movie.Name.1999"},
		{"TV.Show.S01/Season 1/ep1.mkv", "TV.Show.S01"},
		{"singlefile.mkv", "singlefile.mkv"},
		{"", ""},
		{"a/b/c/d.mkv", "a"},
	}
	for _, tt := range tests {
		got := ExtractDirectory(tt.path)
		if got != tt.want {
			t.Errorf("ExtractDirectory(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestExtractRelativePath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"Movie.Name.1999/file.mkv", "file.mkv"},
		{"TV.Show.S01/Season 1/ep1.mkv", "Season 1/ep1.mkv"},
		{"singlefile.mkv", "singlefile.mkv"},
		{"", ""},
	}
	for _, tt := range tests {
		got := ExtractRelativePath(tt.path)
		if got != tt.want {
			t.Errorf("ExtractRelativePath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

var tvRegex = "(?i)(season|episode)s?\\.?\\d?|[se]\\d\\d|\\b(tv|complete)|\\b(saison|stage)\\.?\\d|[a-z]\\s?-\\s?\\d{2,4}\\b|\\d{2,4}\\s?-\\s?\\d{2,4}\\b"

func TestMatchDirectory_Include(t *testing.T) {
	f, err := NewFilter("/tv", tvRegex, "", "", false)
	if err != nil {
		t.Fatalf("NewFilter failed: %v", err)
	}

	shouldMatch := []string{
		"Breaking.Bad.S01.1080p",
		"The.Office.Season.3.Complete",
		"Game.of.Thrones.S08E01",
		"Show.tv.Complete",
	}
	for _, dir := range shouldMatch {
		if !f.MatchDirectory(dir) {
			t.Errorf("include should match %q", dir)
		}
	}

	shouldNotMatch := []string{
		"The.Matrix.1999.1080p",
		"Inception.2010.4K",
	}
	for _, dir := range shouldNotMatch {
		if f.MatchDirectory(dir) {
			t.Errorf("include should NOT match %q", dir)
		}
	}
}

func TestMatchDirectory_Exclude(t *testing.T) {
	f, err := NewFilter("/movies", "", tvRegex, "", false)
	if err != nil {
		t.Fatalf("NewFilter failed: %v", err)
	}

	// Should NOT reject movies
	shouldMatch := []string{
		"The.Matrix.1999.1080p",
		"Inception.2010.4K",
	}
	for _, dir := range shouldMatch {
		if !f.MatchDirectory(dir) {
			t.Errorf("exclude only TV should not reject %q", dir)
		}
	}

	// Should reject TV shows
	shouldNotMatch := []string{
		"Breaking.Bad.S01.1080p",
		"The.Office.Season.3.Complete",
		"Game.of.Thrones.S08E01",
	}
	for _, dir := range shouldNotMatch {
		if f.MatchDirectory(dir) {
			t.Errorf("exclude TV should reject %q", dir)
		}
	}
}

func TestMatchDirectory_IncludeAndExclude(t *testing.T) {
	// Include "season" (must have this to pass), exclude "S01" (must NOT have this)
	f, err := NewFilter("/test", "(?i)season", "(?i)S01", "", false)
	if err != nil {
		t.Fatalf("NewFilter failed: %v", err)
	}
	if !f.MatchDirectory("Show.Season.1") {
		t.Error("Show.Season.1 should match")
	}
	if f.MatchDirectory("Show.Season.1.S01") {
		t.Error("Show.Season.1.S01 should be excluded (matches exclude)")
	}
	if f.MatchDirectory("Movie.2024") {
		t.Error("Movie.2024 should NOT match (no include match)")
	}
}

func TestMatchFile(t *testing.T) {
	f, err := NewFilter("/movies", "", "", `.*\.(mkv|mp4|avi)$`, false)
	if err != nil {
		t.Fatalf("NewFilter failed: %v", err)
	}

	if !f.MatchFile("movie.mkv") {
		t.Error("should match .mkv")
	}
	if !f.MatchFile("show.mp4") {
		t.Error("should match .mp4")
	}
	if !f.MatchFile("clip.avi") {
		t.Error("should match .avi")
	}
	if f.MatchFile("archive.rar") {
		t.Error("should NOT match .rar")
	}
	if f.MatchFile("sample.txt") {
		t.Error("should NOT match .txt")
	}
}

func TestMatchFile_RelativePath(t *testing.T) {
	f, err := NewFilter("/tv", "", "", `.*\.(mkv|mp4)$`, false)
	if err != nil {
		t.Fatalf("NewFilter failed: %v", err)
	}

	if !f.MatchFile("Season 1/episode.mkv") {
		t.Error("should match path with subdirectories")
	}
	if f.MatchFile("Season 1/sample.txt") {
		t.Error("should NOT match non-video in subdirectory")
	}
}

func TestKeepLargest(t *testing.T) {
	records := []metadata.FileRecord{
		{ItemID: 1, Source: metadata.SourceTorrent, Path: "Movie.A/file1.mkv", Size: 500},
		{ItemID: 1, Source: metadata.SourceTorrent, Path: "Movie.A/file2.mkv", Size: 1000},
		{ItemID: 1, Source: metadata.SourceTorrent, Path: "Movie.A/featurette.mkv", Size: 200},
	}
	got := KeepLargest(records)
	if len(got) != 1 {
		t.Fatalf("expected 1 record, got %d", len(got))
	}
	if got[0].Size != 1000 {
		t.Errorf("expected largest file (1000), got %d", got[0].Size)
	}
}

func TestKeepLargest_MultipleItems(t *testing.T) {
	records := []metadata.FileRecord{
		{ItemID: 1, Source: metadata.SourceTorrent, Path: "Movie.A/main.mkv", Size: 1000},
		{ItemID: 1, Source: metadata.SourceTorrent, Path: "Movie.A/featurette.mkv", Size: 200},
		{ItemID: 2, Source: metadata.SourceTorrent, Path: "Show.B/ep1.mkv", Size: 500},
		{ItemID: 2, Source: metadata.SourceTorrent, Path: "Show.B/ep2.mkv", Size: 600},
	}
	got := KeepLargest(records)
	if len(got) != 2 {
		t.Fatalf("expected 2 records, got %d", len(got))
	}
	if got[0].Size != 1000 || got[0].ItemID != 1 {
		t.Errorf("expected item 1 largest (1000), got item %d size %d", got[0].ItemID, got[0].Size)
	}
	if got[1].Size != 600 || got[1].ItemID != 2 {
		t.Errorf("expected item 2 largest (600), got item %d size %d", got[1].ItemID, got[1].Size)
	}
}

func TestKeepLargest_SourceDisambiguation(t *testing.T) {
	records := []metadata.FileRecord{
		{ItemID: 1, Source: metadata.SourceTorrent, Path: "item/file1.mkv", Size: 500},
		{ItemID: 1, Source: metadata.SourceUsenet, Path: "item/file2.mkv", Size: 600},
	}
	got := KeepLargest(records)
	if len(got) != 2 {
		t.Fatalf("expected 2 records (different sources), got %d", len(got))
	}
}

func TestApplyFilter_IncludeOnly(t *testing.T) {
	f, err := NewFilter("/tv", "(?i)S01|season", "", `.*\.(mkv|mp4)$`, false)
	if err != nil {
		t.Fatalf("NewFilter failed: %v", err)
	}
	records := []metadata.FileRecord{
		{ItemID: 1, Source: metadata.SourceTorrent, Path: "TV.Show.S01/ep1.mkv", Size: 1000},
		{ItemID: 2, Source: metadata.SourceTorrent, Path: "The.Matrix.1999/movie.mkv", Size: 5000},
	}
	got := f.Apply(records)
	if len(got) != 1 {
		t.Fatalf("expected 1 record (only TV), got %d", len(got))
	}
	if got[0].ItemID != 1 {
		t.Errorf("expected TV show (item 1), got item %d", got[0].ItemID)
	}
}

func TestApplyFilter_ExcludeOnly(t *testing.T) {
	f, err := NewFilter("/movies", "", "(?i)S01|season", `.*\.(mkv|mp4)$`, false)
	if err != nil {
		t.Fatalf("NewFilter failed: %v", err)
	}
	records := []metadata.FileRecord{
		{ItemID: 1, Source: metadata.SourceTorrent, Path: "TV.Show.S01/ep1.mkv", Size: 1000},
		{ItemID: 2, Source: metadata.SourceTorrent, Path: "The.Matrix.1999/movie.mkv", Size: 5000},
	}
	got := f.Apply(records)
	if len(got) != 1 {
		t.Fatalf("expected 1 record (only movie), got %d", len(got))
	}
	if got[0].ItemID != 2 {
		t.Errorf("expected movie (item 2), got item %d", got[0].ItemID)
	}
}

func TestApplyFilter_LargestFileOnly(t *testing.T) {
	f, err := NewFilter("/movies", "", "", `.*\.(mkv|mp4)$`, true)
	if err != nil {
		t.Fatalf("NewFilter failed: %v", err)
	}
	records := []metadata.FileRecord{
		{ItemID: 1, Source: metadata.SourceTorrent, Path: "Movie/file.mkv", Size: 5000},
		{ItemID: 1, Source: metadata.SourceTorrent, Path: "Movie/featurette.mkv", Size: 200},
	}
	got := f.Apply(records)
	if len(got) != 1 {
		t.Fatalf("expected 1 record (largest), got %d", len(got))
	}
	if got[0].Size != 5000 {
		t.Errorf("expected largest (5000), got %d", got[0].Size)
	}
}

func TestMatchSize(t *testing.T) {
	f := &Filter{MinSize: 100, MaxSize: 1000}
	if !f.MatchSize(100) || !f.MatchSize(500) || !f.MatchSize(1000) {
		t.Error("sizes in range should match")
	}
	if f.MatchSize(99) || f.MatchSize(1001) {
		t.Error("sizes out of range should not match")
	}
	// Zero bounds = unlimited.
	open := &Filter{}
	if !open.MatchSize(0) || !open.MatchSize(1<<40) {
		t.Error("zero min/max should accept any size")
	}
}

func TestApplyFilter_MinMaxFileSize(t *testing.T) {
	f, err := NewFilter("/movies", "", "", `.*\.mkv$`, false)
	if err != nil {
		t.Fatalf("NewFilter failed: %v", err)
	}
	f.MinSize = 300
	f.MaxSize = 5000

	records := []metadata.FileRecord{
		{ItemID: 1, Path: "A/sample.mkv", Size: 50},
		{ItemID: 2, Path: "B/main.mkv", Size: 1000},
		{ItemID: 3, Path: "C/remux.mkv", Size: 9000},
		{ItemID: 4, Path: "D/ok.mkv", Size: 5000},
	}
	got := f.Apply(records)
	if len(got) != 2 {
		t.Fatalf("expected 2 records in range, got %d", len(got))
	}
	if got[0].ItemID != 2 || got[1].ItemID != 4 {
		t.Errorf("unexpected items: %+v", got)
	}
}

func TestApplyFilter_SizeThenLargest(t *testing.T) {
	// Sample under min is dropped before largest_file_only picks a winner.
	f, err := NewFilter("/movies", "", "", `.*\.mkv$`, true)
	if err != nil {
		t.Fatalf("NewFilter failed: %v", err)
	}
	f.MinSize = 400

	records := []metadata.FileRecord{
		{ItemID: 1, Source: metadata.SourceTorrent, Path: "Movie/sample.mkv", Size: 100},
		{ItemID: 1, Source: metadata.SourceTorrent, Path: "Movie/feature.mkv", Size: 2000},
		{ItemID: 1, Source: metadata.SourceTorrent, Path: "Movie/extra.mkv", Size: 500},
	}
	got := f.Apply(records)
	if len(got) != 1 {
		t.Fatalf("expected 1 record, got %d", len(got))
	}
	if got[0].Size != 2000 {
		t.Errorf("expected feature (2000) after size filter + largest, got %d", got[0].Size)
	}
}
