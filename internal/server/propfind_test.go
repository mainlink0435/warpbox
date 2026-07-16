package server

import (
	"encoding/xml"
	"net/url"
	"testing"
)

func TestMultiStatusXMLFormat(t *testing.T) {
	// Build a response with one directory and one file.
	responses := []response{
		{
			Href: "/webdav/",
			PropStat: propstat{
				Prop: prop{
					DisplayName:  "webdav",
					ResourceType: &resourceType{Collection: &collection{}},
				},
				Status: "HTTP/1.1 200 OK",
			},
		},
		{
			Href: "/webdav/movie.mkv",
			PropStat: propstat{
				Prop: prop{
					DisplayName:   "movie.mkv",
					ContentLength: 4294967296,
					ContentType:   "video/x-matroska",
					LastModified:  "Mon, 01 Jan 2024 00:00:00 GMT",
				},
				Status: "HTTP/1.1 200 OK",
			},
		},
	}

	ms := multiStatus{
		XmlnsD:    davNamespace,
		Responses: responses,
	}

	output, err := xml.MarshalIndent(ms, "", "  ")
	if err != nil {
		t.Fatalf("XML marshal failed: %v", err)
	}

	full := append([]byte(xml.Header), output...)

	// Verify key parts of the output.
	s := string(full)
	cases := []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<D:multistatus`,
		`xmlns:D="DAV:"`,
		`<D:href>/webdav/</D:href>`,
		`<D:displayname>webdav</D:displayname>`,
		`<D:collection></D:collection>`,
		`<D:href>/webdav/movie.mkv</D:href>`,
		`<D:getcontentlength>4294967296</D:getcontentlength>`,
		`<D:getcontenttype>video/x-matroska</D:getcontenttype>`,
		`<D:getlastmodified>Mon, 01 Jan 2024 00:00:00 GMT</D:getlastmodified>`,
		`<D:status>HTTP/1.1 200 OK</D:status>`,
		`</D:multistatus>`,
	}

	for _, c := range cases {
		if !contains(s, c) {
			t.Errorf("Expected output to contain:\n%s\n\nGot:\n%s", c, s)
		}
	}
}

func TestEmptyMultiStatus(t *testing.T) {
	ms := multiStatus{
		XmlnsD:    davNamespace,
		Responses: []response{},
	}

	output, err := xml.MarshalIndent(ms, "", "  ")
	if err != nil {
		t.Fatalf("XML marshal failed: %v", err)
	}

	s := string(output)
	if !contains(s, `<D:multistatus`) {
		t.Errorf("Expected multiStatus element, got: %s", s)
	}
	if !contains(s, `</D:multistatus>`) {
		t.Errorf("Expected closing multiStatus element, got: %s", s)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsStr(s, substr)
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestEncodeDAVHref(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"/webdav/", "/webdav/"},
		{"/webdav/movie.mkv", "/webdav/movie.mkv"},
		{"/webdav/Season 10/", "/webdav/Season%2010/"},
		{
			"/webdav/tv/Show/30% Iron Chef.mkv",
			"/webdav/tv/Show/30%25%20Iron%20Chef.mkv",
		},
		{
			"/webdav/Heroes (2006)/.07% (1080p).mkv",
			"/webdav/Heroes%20%282006%29/.07%25%20%281080p%29.mkv",
		},
		{
			"/webdav/Robot Chicken (2001)/Season 10/",
			"/webdav/Robot%20Chicken%20%282001%29/Season%2010/",
		},
	}
	for _, tc := range cases {
		got := encodeDAVHref(tc.in)
		if got != tc.want {
			t.Errorf("encodeDAVHref(%q) = %q, want %q", tc.in, got, tc.want)
		}
		// Encoded form must be parseable as a URL path (the rclone failure mode).
		if tc.in != "" {
			if _, err := url.Parse(got); err != nil {
				t.Errorf("encodeDAVHref(%q) = %q is not a valid URL: %v", tc.in, got, err)
			}
		}
	}
}

func TestAppendResponseEncodesHrefKeepsDisplayName(t *testing.T) {
	seen := map[string]bool{}
	raw := "/webdav/tv/Futurama/30% Iron Chef.mkv"
	responses := appendResponse(nil, raw, false, 100, "30% Iron Chef.mkv", "video/mp4", "", &seen)

	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	wantHref := "/webdav/tv/Futurama/30%25%20Iron%20Chef.mkv"
	if responses[0].Href != wantHref {
		t.Errorf("Href = %q, want %q", responses[0].Href, wantHref)
	}
	if responses[0].PropStat.Prop.DisplayName != "30% Iron Chef.mkv" {
		t.Errorf("DisplayName = %q, want literal percent", responses[0].PropStat.Prop.DisplayName)
	}

	// Dedup key is the unencoded path — second append is a no-op.
	responses = appendResponse(responses, raw, false, 100, "30% Iron Chef.mkv", "video/mp4", "", &seen)
	if len(responses) != 1 {
		t.Errorf("dedup by unencoded href failed: got %d responses", len(responses))
	}

	// Directory display name stays human-readable.
	seen2 := map[string]bool{}
	dirRaw := "/webdav/Season 10/"
	dirResps := appendResponse(nil, dirRaw, true, 0, "", "", "", &seen2)
	if dirResps[0].Href != "/webdav/Season%2010/" {
		t.Errorf("dir Href = %q, want encoded", dirResps[0].Href)
	}
	if dirResps[0].PropStat.Prop.DisplayName != "Season 10" {
		t.Errorf("dir DisplayName = %q, want %q", dirResps[0].PropStat.Prop.DisplayName, "Season 10")
	}
}