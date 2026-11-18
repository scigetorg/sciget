package main

import (
	"compress/gzip"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// maxSize is the catalog size limit the refresh tests pass.
const maxSize = 1024

func TestRefreshCatalog(t *testing.T) {
	const catalog = `{"fsl": {"apps": {"fsl 6.0.3": {"version": "20200905"}}}}`
	const other = `{"fsl": {"apps": {"fsl 6.0.4": {"version": "20210101"}}}}`
	tests := []struct {
		name        string
		existing    string // apps.json before the refresh; no file when empty
		blocked     bool   // the directory is read-only, so apps.json cannot be replaced
		urlBlocked  bool   // url is a directory, so writing it fails
		status      int    // the server's status; 200 when zero
		body        string
		gzip        bool // the server compresses the body
		short       bool // the server promises more bytes than it sends
		large       bool // the server sends one byte more than maxSize
		closed      bool // the server is gone before the request
		wantChanged bool
		want        string // apps.json afterwards
		wantErr     string
	}{
		{name: "first refresh", body: catalog, wantChanged: true, want: catalog},
		{name: "same catalog, so only url is written", existing: catalog, body: catalog, want: catalog},
		{name: "different catalog", existing: catalog, body: other, wantChanged: true, want: other},
		{name: "damaged local copy is replaced", existing: "{}", body: catalog, wantChanged: true,
			want: catalog},
		{name: "compressed", body: catalog, gzip: true, wantChanged: true, want: catalog},
		{name: "not found", existing: catalog, status: http.StatusNotFound, want: catalog,
			wantErr: "returned 404 Not Found"},
		{name: "server error", existing: catalog, status: http.StatusInternalServerError, want: catalog,
			wantErr: "returned 500 Internal Server Error"},
		{name: "not modified", existing: catalog, status: http.StatusNotModified, want: catalog,
			wantErr: "returned 304 Not Modified"},
		{name: "invalid JSON", existing: catalog, body: "{", want: catalog,
			wantErr: "downloaded catalog is invalid: unexpected end of JSON input"},
		{name: "no containers", existing: catalog, body: "{}", want: catalog,
			wantErr: "downloaded catalog is invalid: no containers"},
		{name: "write fails", existing: catalog, body: other, blocked: true, want: catalog,
			wantErr: "writing catalog: "},
		{name: "url write fails", existing: catalog, body: other, urlBlocked: true, want: catalog,
			wantErr: "writing catalog: "},
		{name: "larger than the cap", existing: catalog, large: true, want: catalog,
			wantErr: "is larger than max_catalog_size (1024 bytes)"},
		{name: "body cut short", existing: catalog, body: catalog, short: true, want: catalog,
			wantErr: "downloading catalog: unexpected EOF"},
		{name: "server gone", existing: catalog, closed: true, want: catalog,
			wantErr: "downloading catalog: Get "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests []*http.Request
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r)
				switch {
				case tt.status != 0:
					w.WriteHeader(tt.status)
				case tt.gzip:
					w.Header().Set("Content-Encoding", "gzip")
					zw := gzip.NewWriter(w)
					zw.Write([]byte(tt.body))
					zw.Close()
				case tt.large:
					w.Write(make([]byte, maxSize+1))
				case tt.short:
					w.Header().Set("Content-Length", "1000")
					w.Write([]byte(tt.body))
				default:
					w.Write([]byte(tt.body))
				}
			}))
			defer srv.Close()
			if tt.closed {
				srv.Close()
			}

			dir := t.TempDir()
			path := filepath.Join(dir, "apps.json")
			old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			if tt.existing != "" {
				if err := os.WriteFile(path, []byte(tt.existing), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
			}
			urlPath := filepath.Join(dir, "url")
			if tt.blocked {
				if runtime.GOOS == "windows" || os.Geteuid() == 0 {
					t.Skip("needs a directory the test cannot write to")
				}
				// url already exists, so only the new apps.json fails.
				if err := os.WriteFile(urlPath, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(dir, 0o555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(dir, 0o755) })
			}
			if tt.urlBlocked {
				if err := os.Mkdir(urlPath, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			catalogURL := srv.URL + "/apps.json"
			changed, err := refreshCatalog(srv.Client(), catalogURL, dir, maxSize)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				if tt.status != 0 && (err == nil || !strings.Contains(err.Error(), catalogURL)) {
					t.Errorf("error = %v, want it to name %s", err, catalogURL)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
			}
			// url is written once the download validates, before apps.json.
			urlData, urlErr := os.ReadFile(urlPath)
			switch {
			case tt.urlBlocked:
			case tt.wantErr == "" || tt.blocked:
				if string(urlData) != catalogURL+"\n" {
					t.Errorf("url = %q, %v, want %q", urlData, urlErr, catalogURL+"\n")
				}
			case !errors.Is(urlErr, os.ErrNotExist):
				t.Errorf("url written after a failed download: %q, %v", urlData, urlErr)
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tt.want {
				t.Errorf("apps.json = %q, want %q", data, tt.want)
			}
			if tt.existing != "" && !tt.wantChanged {
				if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(old) {
					t.Errorf("apps.json rewritten: %v", err)
				}
			}
			if left, _ := filepath.Glob(filepath.Join(dir, "apps.json.*.incomplete")); len(left) > 0 {
				t.Errorf("temporary files left behind: %v", left)
			}
			if tt.wantChanged {

				// A second refresh must see what the first wrote as the same catalog.
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
				changed, err := refreshCatalog(srv.Client(), catalogURL, dir, maxSize)
				if err != nil || changed {
					t.Errorf("second refresh: changed = %v, error = %v, want no change", changed, err)
				}
				if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(old) {
					t.Errorf("second refresh rewrote apps.json: %v", err)
				}
			}
			wantRequests := 1
			switch {
			case tt.closed:
				wantRequests = 0
			case tt.wantChanged:
				wantRequests = 2 // the second refresh above
			}
			if len(requests) != wantRequests {
				t.Errorf("server saw %d requests, want %d", len(requests), wantRequests)
			}
			for _, r := range requests {
				if ua := r.Header.Get("User-Agent"); ua != "sciget/dev" {
					t.Errorf("User-Agent = %q, want sciget/dev", ua)
				}
				for _, h := range []string{"If-None-Match", "If-Modified-Since"} {
					if v := r.Header.Get(h); v != "" {
						t.Errorf("%s = %q, want none", h, v)
					}
				}
			}
		})
	}
}
