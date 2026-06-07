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
	"path"
	"strings"
	"time"
)

const davNamespace = "DAV:"

type multiStatus struct {
	XMLName   xml.Name   `xml:"D:multistatus"`
	XmlnsD    string     `xml:"xmlns:D,attr"`
	Responses []response `xml:"D:response"`
}

type response struct {
	Href     string    `xml:"D:href"`
	PropStat propstat  `xml:"D:propstat"`
}

type propstat struct {
	Prop   prop   `xml:"D:prop"`
	Status string `xml:"D:status"`
}

type prop struct {
	DisplayName     string        `xml:"D:displayname"`
	ContentLength   int64         `xml:"D:getcontentlength,omitempty"`
	ContentType     string        `xml:"D:getcontenttype,omitempty"`
	LastModified    string        `xml:"D:getlastmodified,omitempty"`
	ResourceType    *resourceType `xml:"D:resourcetype,omitempty"`
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

	// Build the virtual prefix: strip the WebDAV root from the path.
	prefix := strings.TrimPrefix(reqPath, s.root)
	prefix = strings.TrimPrefix(prefix, "/")

	// List files from SQLite matching this prefix.
	records, err := s.store.ListDir(prefix)
	if err != nil {
		slog.Error("PROPFIND: ListDir failed", "prefix", prefix, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Build a set of virtual paths for the response.
	seen := map[string]bool{}
	var responses []response

	// Always include the requested directory itself.
	dirHref := reqPath
	if !strings.HasSuffix(dirHref, "/") {
		dirHref += "/"
	}
	responses = appendResponse(responses, dirHref, true, 0, "", "", &seen)

	// Add immediate children based on depth.
	if depth == "1" || depth == "infinity" {
		for _, rec := range records {
			// Build the full WebDAV href for this file.
			relPath := strings.TrimPrefix(rec.Path, prefix)
			relPath = strings.TrimPrefix(relPath, "/")
			childHref := strings.TrimRight(reqPath, "/") + "/" + relPath

			if !seen[childHref] {
				// Determine MIME type; default to octet-stream for files.
				mime := rec.MimeType
				if mime == "" {
					mime = "application/octet-stream"
				}
				responses = appendResponse(responses, childHref, false, rec.Size, rec.Name, mime, &seen)
			}
		}
	}

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
func appendResponse(responses []response, href string, isDir bool, size int64, name, mimeType string, seen *map[string]bool) []response {
	if (*seen)[href] {
		return responses
	}
	(*seen)[href] = true

	var p prop
	if isDir {
		p = prop{
			DisplayName:  path.Base(href),
			ResourceType: &resourceType{Collection: &collection{}},
		}
	} else {
		p = prop{
			DisplayName:   name,
			ContentLength: size,
			ContentType:   mimeType,
			LastModified:  time.Now().UTC().Format(http.TimeFormat),
		}
	}

	return append(responses, response{
		Href: href,
		PropStat: propstat{
			Prop:   p,
			Status: "HTTP/1.1 200 OK",
		},
	})
}
