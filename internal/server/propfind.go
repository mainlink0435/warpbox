// WebDAV PROPFIND handler — serves directory listings from the SQLite metadata store.
//
// This is the zero-API-cost browsing layer. rclone sends a PROPFIND request,
// Warpbox responds with a Multi-Status XML document listing the files in the
// requested virtual directory. No TorBox API calls are made.
package server

import (
	"encoding/xml"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/mainlink0435/warpbox/internal/library"
)

// encodeDAVHref percent-encodes each segment of an absolute path so it is a
// valid URI-reference for WebDAV D:href (and HTTP browser links). Slash
// separators are preserved; a trailing slash is kept. Stored SQLite paths and
// display names remain unencoded — this is only for wire format.
//
// Without this, literal '%' in titles (e.g. "30% Iron Chef") produces invalid
// URL escapes that break rclone ("invalid URL escape \"% o\"").
func encodeDAVHref(p string) string {
	if p == "" {
		return p
	}
	trailing := strings.HasSuffix(p, "/")
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	out := strings.Join(parts, "/")
	if trailing && !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out
}

// formatLastModified parses an ISO 8601 timestamp (like TorBox API's created_at)
// and returns an RFC 1123-formatted string suitable for WebDAV getlastmodified.
// If the input is empty or unparseable, it returns the current time.
func formatLastModified(createdAt string) string {
	if createdAt != "" {
		t, err := time.Parse("2006-01-02T15:04:05", createdAt[:19])
		if err == nil {
			return t.UTC().Format(http.TimeFormat)
		}
	}
	return time.Now().UTC().Format(http.TimeFormat)
}

const davNamespace = "DAV:"

type multiStatus struct {
	XMLName   xml.Name   `xml:"D:multistatus"`
	XmlnsD    string     `xml:"xmlns:D,attr"`
	Responses []response `xml:"D:response"`
}

type response struct {
	Href     string   `xml:"D:href"`
	PropStat propstat `xml:"D:propstat"`
}

type propstat struct {
	Prop   prop   `xml:"D:prop"`
	Status string `xml:"D:status"`
}

type prop struct {
	DisplayName   string        `xml:"D:displayname"`
	ContentLength int64         `xml:"D:getcontentlength,omitempty"`
	ContentType   string        `xml:"D:getcontenttype,omitempty"`
	LastModified  string        `xml:"D:getlastmodified,omitempty"`
	ResourceType  *resourceType `xml:"D:resourcetype,omitempty"`
}

type resourceType struct {
	Collection *collection `xml:"D:collection,omitempty"`
}

type collection struct{}

// ---------------------------------------------------------------------------
// PROPFIND handler
// ---------------------------------------------------------------------------

func (s *Server) handlePropfind(w http.ResponseWriter, r *http.Request) {
	// Resolve the virtual path from the request URL.
	reqPath := strings.TrimRight(r.URL.Path, "/")
	if reqPath == "" {
		reqPath = "/"
	}

	slog.Debug("PROPFIND", "depth", r.Header.Get("Depth"), "path", reqPath)

	// Determine depth.
	depth := r.Header.Get("Depth")
	if depth == "" {
		depth = "1" // RFC 4918 default
	}

	// Build the virtual prefix: strip the WebDAV root (or mount) from the path.
	prefix := strings.TrimPrefix(reqPath, s.rootForRequest(r))
	prefix = strings.TrimPrefix(prefix, "/")

	// List files from SQLite matching this prefix.
	records, err := s.store.ListDir(prefix)
	if err != nil {
		slog.Error("PROPFIND: ListDir failed", "prefix", prefix, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Apply library filter if present in request context.
	if f, ok := r.Context().Value(filterKey).(*library.Filter); ok && f != nil {
		records = f.Apply(records)
	}

	// Build a set of virtual paths for the response.
	seen := map[string]bool{}
	var responses []response

	// Always include the requested directory itself.
	dirHref := reqPath
	if !strings.HasSuffix(dirHref, "/") {
		dirHref += "/"
	}
	responses = appendResponse(responses, dirHref, true, 0, "", "", "", &seen)

	// Add immediate children based on depth.
	if depth == "1" || depth == "infinity" {

		// At the root level (/webdav/) with virtual paths configured,
		// show synthetic directory entries instead of real files.
		_, isInsideMount := r.Context().Value(mountRootKey).(string)
		if prefix == "" && !isInsideMount {
			baseHref := strings.TrimRight(reqPath, "/") + "/"
			// __all__ is always first.
			responses = appendResponse(responses, baseHref+"__all__/", true, 0, "", "", "", &seen)
			for _, vf := range s.virtualFilters {
				name := strings.TrimPrefix(vf.Mount, "/")
				responses = appendResponse(responses, baseHref+name+"/", true, 0, "", "", "", &seen)
			}
		} else {
			// Standard directory listing from file records.
			type childInfo struct {
				isDir     bool
				size      int64
				name      string
				mime      string
				createdAt string
			}
			immediate := map[string]childInfo{}

			for _, rec := range records {
			relPath := strings.TrimPrefix(rec.Path, prefix)
			relPath = strings.TrimPrefix(relPath, "/")

			parts := strings.SplitN(relPath, "/", 2)
			immediateName := parts[0]

			if _, exists := immediate[immediateName]; exists {
				continue
			}

			if len(parts) > 1 {
				// The file is nested deeper — the immediate child is a directory.
				// Use the first file's created_at as the directory timestamp.
				if existing, ok := immediate[immediateName]; !ok || existing.createdAt == "" {
					immediate[immediateName] = childInfo{
						isDir:     true,
						createdAt: rec.CreatedAt,
					}
				}
			} else {
				// Direct file in the requested directory.
				mime := rec.MimeType
				if mime == "" {
					mime = "application/octet-stream"
				}
				immediate[immediateName] = childInfo{
					isDir:     false,
					size:      rec.Size,
					name:      rec.Name,
					mime:      mime,
					createdAt: rec.CreatedAt,
				}
			}
		}

		// Build response entries from the immediate children map.
		baseHref := strings.TrimRight(reqPath, "/") + "/"
		for name, info := range immediate {
			childHref := baseHref + name
			if info.isDir {
				childHref += "/"
				responses = appendResponse(responses, childHref, true, 0, "", "", info.createdAt, &seen)
			} else {
				responses = appendResponse(responses, childHref, false, info.size, info.name, info.mime, info.createdAt, &seen)
			}
		}
		} // close else block
	} // close depth block

	// Build the XML response.
	ms := multiStatus{
		XmlnsD:    davNamespace,
		Responses: responses,
	}

	output, err := xml.MarshalIndent(ms, "", "  ")
	if err != nil {
		slog.Error("PROPFIND: XML marshal failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Prepend XML declaration.
	body := append([]byte(xml.Header), output...)

	w.Header().Set("DAV", "1")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	w.Write(body)
}

// appendResponse creates a WebDAV response entry and appends it to the slice.
// href is the unencoded virtual path used for display-name derivation and
// deduplication; the emitted D:href is percent-encoded per segment.
func appendResponse(responses []response, href string, isDir bool, size int64, name, mimeType, createdAt string, seen *map[string]bool) []response {
	if (*seen)[href] {
		return responses
	}
	(*seen)[href] = true

	var p prop
	if isDir {
		p = prop{
			DisplayName:  path.Base(href),
			ResourceType: &resourceType{Collection: &collection{}},
			LastModified: formatLastModified(createdAt),
		}
	} else {
		p = prop{
			DisplayName:   name,
			ContentLength: size,
			ContentType:   mimeType,
			LastModified:  formatLastModified(createdAt),
		}
	}

	return append(responses, response{
		Href: encodeDAVHref(href),
		PropStat: propstat{
			Prop:   p,
			Status: "HTTP/1.1 200 OK",
		},
	})
}
