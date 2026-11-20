package main

import (
	"cmp"
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
	const apps = `{"fsl": {"apps": {"fsl 6.0.3": {"version": "20200905"}}}}`
	const other = `{"fsl": {"apps": {"fsl 6.0.4": {"version": "20210101"}}}}`
	const domains = `{"schemaVersion": 1, "domains": {"bad": 5, "neuro": {"appJsonUrl": "apps.json", "downloadUrls": ["https://example.org/{file}"], "registry": "x"}}}`
	const neuroJSON = `{"appJsonUrl":"apps.json","downloadUrls":["https://example.org/{file}"],"registry":"x","schemaVersion":1}`
	tests := []struct {
		name        string
		catalogJSON string // served at /catalog.json; testCatalogJSON when empty
		domain      string // the catalog's domain
		wantJSON    string // catalog.json afterwards, when set
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
		{name: "first refresh", body: apps, wantChanged: true, want: apps},
		{name: "same catalog, so only url is written", existing: apps, body: apps, want: apps},
		{name: "different catalog", existing: apps, body: other, wantChanged: true, want: other},
		{name: "damaged local copy is replaced", existing: "{}", body: apps, wantChanged: true,
			want: apps},
		{name: "compressed", body: apps, gzip: true, wantChanged: true, want: apps},
		{name: "not found", existing: apps, status: http.StatusNotFound, want: apps,
			wantErr: "returned 404 Not Found"},
		{name: "server error", existing: apps, status: http.StatusInternalServerError, want: apps,
			wantErr: "returned 500 Internal Server Error"},
		{name: "not modified", existing: apps, status: http.StatusNotModified, want: apps,
			wantErr: "returned 304 Not Modified"},
		{name: "invalid JSON", existing: apps, body: "{", want: apps,
			wantErr: "downloaded catalog is invalid: unexpected end of JSON input"},
		{name: "no containers", existing: apps, body: "{}", want: apps,
			wantErr: "downloaded catalog is invalid: no containers"},
		{name: "write fails", existing: apps, body: other, blocked: true, want: apps,
			wantErr: "writing catalog: "},
		{name: "url write fails", existing: apps, body: other, urlBlocked: true, want: apps,
			wantErr: "writing catalog: "},
		{name: "larger than the cap", existing: apps, large: true, want: apps,
			wantErr: "is larger than max_catalog_size (1024 bytes)"},
		{name: "body cut short", existing: apps, body: apps, short: true, want: apps,
			wantErr: "downloading catalog: unexpected EOF"},
		{name: "server gone", existing: apps, closed: true, want: apps,
			wantErr: "downloading catalog: Get "},
		{name: "a domain of a domains file", catalogJSON: domains, domain: "neuro", body: apps,
			wantChanged: true, want: apps, wantJSON: neuroJSON},
		{name: "a domain gone from the file", catalogJSON: domains, domain: "eeg", existing: apps, body: other,
			want: apps, wantErr: `domain "eeg" is no longer in http://`},
		{name: "a domain that is not an object", catalogJSON: domains, domain: "bad", existing: apps, body: other,
			want: apps, wantErr: "is not a valid catalog JSON: json: cannot unmarshal number"},
		{name: "an invalid catalog JSON", catalogJSON: `{"appJsonUrl": "apps.json"}`, existing: apps, body: other,
			want: apps, wantErr: "is not a valid catalog JSON: schemaVersion must be 1"},
		{name: "a domains file with no domain", catalogJSON: domains, existing: apps, body: other,
			want: apps, wantErr: "is not a valid catalog JSON: no appJsonUrl"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests []*http.Request
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/catalog.json" {
					w.Write([]byte(cmp.Or(tt.catalogJSON, testCatalogJSON)))
					return
				}
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
			jsonPath := filepath.Join(dir, "catalog.json")
			if tt.existing != "" {
				if err := os.WriteFile(path, []byte(tt.existing), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(jsonPath, []byte(cmp.Or(tt.catalogJSON, testCatalogJSON)), 0o600); err != nil {
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

			catalogURL := srv.URL + "/catalog.json"
			cat := catalog{Name: "neuro", URL: catalogURL, Domain: tt.domain}
			changed, err := refreshCatalog(onceReader(srv.Client(), maxSize), cat, dir)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				if tt.status != 0 && (err == nil || !strings.Contains(err.Error(), srv.URL+"/apps.json")) {
					t.Errorf("error = %v, want it to name %s", err, srv.URL+"/apps.json")
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
			if tt.wantJSON != "" {
				if data, err := os.ReadFile(jsonPath); err != nil || string(data) != tt.wantJSON {
					t.Errorf("catalog.json = %q, %v, want %q", data, err, tt.wantJSON)
				}
			}
			if info, err := os.Stat(jsonPath); tt.wantChanged && runtime.GOOS != "windows" && (err != nil || info.Mode().Perm() != 0o600) {
				t.Errorf("catalog.json mode = %v, %v, want 0600", info, err)
			}
			if left, _ := filepath.Glob(filepath.Join(dir, "*.incomplete")); len(left) > 0 {
				t.Errorf("temporary files left behind: %v", left)
			}
			if tt.wantChanged {

				// A second refresh must see what the first wrote as the same catalog.
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
				changed, err := refreshCatalog(onceReader(srv.Client(), maxSize), cat, dir)
				if err != nil || changed {
					t.Errorf("second refresh: changed = %v, error = %v, want no change", changed, err)
				}
				if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(old) {
					t.Errorf("second refresh rewrote apps.json: %v", err)
				}
			}
			wantRequests := 1
			switch {
			case tt.closed, tt.catalogJSON != "" && tt.wantErr != "": // apps.json is never asked for
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
