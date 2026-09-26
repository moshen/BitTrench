package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddRequestReadsAFileAndPassesURIsThrough(t *testing.T) {
	torrentFile := filepath.Join(t.TempDir(), "example.torrent")
	if err := os.WriteFile(torrentFile, []byte("d4:infod4:name7:exampleee"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		source       string
		wantSource   string
		wantMetainfo string
	}{
		{name: "magnet", source: "magnet:?xt=urn:btih:abc", wantSource: "magnet:?xt=urn:btih:abc"},
		{name: "http", source: "http://example.invalid/x.torrent", wantSource: "http://example.invalid/x.torrent"},
		{name: "https", source: "https://example.invalid/x.torrent", wantSource: "https://example.invalid/x.torrent"},
		{name: "local file", source: torrentFile, wantMetainfo: "d4:infod4:name7:exampleee"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := addRequest(tc.source, "/downloads")
			if err != nil {
				t.Fatalf("addRequest: %v", err)
			}
			if req.Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", req.Source, tc.wantSource)
			}
			if string(req.Metainfo) != tc.wantMetainfo {
				t.Errorf("Metainfo = %q, want %q", req.Metainfo, tc.wantMetainfo)
			}
			// The engine's spec() takes exactly one of the two.
			if (req.Source == "") == (len(req.Metainfo) == 0) {
				t.Errorf("exactly one of Source and Metainfo must be set, got %q and %d bytes",
					req.Source, len(req.Metainfo))
			}
			if req.DownloadDir != "/downloads" {
				t.Errorf("DownloadDir = %q, want /downloads", req.DownloadDir)
			}
		})
	}
}

// A path that is not there has to say so by name: it is the one argument the
// command takes, and a typo is the likeliest way to get this wrong.
func TestAddRequestReportsAMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.torrent")
	_, err := addRequest(missing, "")
	if err == nil {
		t.Fatal("expected an error for a file that does not exist")
	}
	if got := err.Error(); !strings.Contains(got, missing) {
		t.Errorf("error %q should name %q", got, missing)
	}
}

func TestPercent(t *testing.T) {
	tests := []struct {
		completed, total int64
		want             int
	}{
		{0, 0, 0}, // before metadata: a zero total is normal, not an error
		{0, 100, 0},
		{50, 100, 50},
		{100, 100, 100},
		{1, 3, 33},
		{5, 0, 0},
	}
	for _, tc := range tests {
		if got := percent(tc.completed, tc.total); got != tc.want {
			t.Errorf("percent(%d, %d) = %d, want %d", tc.completed, tc.total, got, tc.want)
		}
	}
}
