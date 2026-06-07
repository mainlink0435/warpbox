package server

import (
	"encoding/xml"
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