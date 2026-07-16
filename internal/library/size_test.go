package library

import (
	"testing"
)

func TestParseFileSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"  ", 0, false},
		{"0", 0, false},
		{"123", 123, false},
		{"300KB", 300 * 1024, false},
		{"300kb", 300 * 1024, false},
		{"300K", 300 * 1024, false},
		{"300MB", 300 * 1024 * 1024, false},
		{"300 MB", 300 * 1024 * 1024, false},
		{"10GB", 10 * 1024 * 1024 * 1024, false},
		{"10G", 10 * 1024 * 1024 * 1024, false},
		{"1.5GB", int64(1.5 * 1024 * 1024 * 1024), false},
		{"1TB", 1024 * 1024 * 1024 * 1024, false},
		{"1GiB", 1024 * 1024 * 1024, false},
		{"-1MB", 0, true},
		{"abc", 0, true},
		{"10XB", 0, true},
		{"MB", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseFileSize(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseFileSize(%q) expected error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseFileSize(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseFileSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
