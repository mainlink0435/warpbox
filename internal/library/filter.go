package library

import (
	"regexp"
	"strings"

	"github.com/mainlink0435/warpbox/internal/metadata"
)

type Filter struct {
	Mount            string
	DirectoryInclude *regexp.Regexp
	DirectoryExclude *regexp.Regexp
	FileRegex        *regexp.Regexp
	LargestFileOnly  bool
	// MinSize / MaxSize are byte bounds applied after name filters.
	// Zero means no bound (unlimited).
	MinSize int64
	MaxSize int64
}

func NewFilter(mount, dirInclude, dirExclude, fileRegex string, largestFileOnly bool) (*Filter, error) {
	f := &Filter{Mount: mount, LargestFileOnly: largestFileOnly}
	if dirInclude != "" {
		r, err := regexp.Compile(dirInclude)
		if err != nil {
			return nil, err
		}
		f.DirectoryInclude = r
	}
	if dirExclude != "" {
		r, err := regexp.Compile(dirExclude)
		if err != nil {
			return nil, err
		}
		f.DirectoryExclude = r
	}
	if fileRegex != "" {
		r, err := regexp.Compile(fileRegex)
		if err != nil {
			return nil, err
		}
		f.FileRegex = r
	}
	return f, nil
}

func ExtractDirectory(path string) string {
	if idx := strings.IndexByte(path, '/'); idx >= 0 {
		return path[:idx]
	}
	return path
}

func ExtractRelativePath(path string) string {
	if idx := strings.IndexByte(path, '/'); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

func (f *Filter) MatchDirectory(name string) bool {
	if f.DirectoryInclude != nil && !f.DirectoryInclude.MatchString(name) {
		return false
	}
	if f.DirectoryExclude != nil && f.DirectoryExclude.MatchString(name) {
		return false
	}
	return true
}

func (f *Filter) MatchFile(relPath string) bool {
	if f.FileRegex == nil {
		return true
	}
	return f.FileRegex.MatchString(relPath)
}

// MatchSize reports whether size (bytes) is within configured min/max bounds.
// A zero MinSize or MaxSize means that bound is not applied.
func (f *Filter) MatchSize(size int64) bool {
	if f.MinSize > 0 && size < f.MinSize {
		return false
	}
	if f.MaxSize > 0 && size > f.MaxSize {
		return false
	}
	return true
}

func (f *Filter) Apply(records []metadata.FileRecord) []metadata.FileRecord {
	dirMatchCache := make(map[string]bool, len(records)/2)
	result := make([]metadata.FileRecord, 0, len(records))

	for _, rec := range records {
		dir := ExtractDirectory(rec.Path)
		ok, cached := dirMatchCache[dir]
		if !cached {
			ok = f.MatchDirectory(dir)
			dirMatchCache[dir] = ok
		}
		if !ok {
			continue
		}
		rel := ExtractRelativePath(rec.Path)
		if !f.MatchFile(rel) {
			continue
		}
		// Size bounds before largest_file_only so samples under min drop first.
		if !f.MatchSize(rec.Size) {
			continue
		}
		result = append(result, rec)
	}

	if f.LargestFileOnly {
		result = KeepLargest(result)
	}

	return result
}

func KeepLargest(records []metadata.FileRecord) []metadata.FileRecord {
	type key struct {
		source metadata.FileSource
		itemID int64
	}
	best := make(map[key]metadata.FileRecord)
	order := make([]key, 0, len(records)/2)
	for _, rec := range records {
		k := key{source: rec.Source, itemID: rec.ItemID}
		existing, has := best[k]
		if !has {
			best[k] = rec
			order = append(order, k)
		} else if rec.Size > existing.Size {
			best[k] = rec
		}
	}
	result := make([]metadata.FileRecord, 0, len(order))
	for _, k := range order {
		result = append(result, best[k])
	}
	return result
}
