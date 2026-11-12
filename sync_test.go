package main

import (
	"compress/gzip"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSyncCatalogue(t *testing.T) {
	const catalogue = `{"fsl": {"apps": {"fsl 6.0.3": {"version": "20200905"}}}}`
	const other = `{"fsl": {"apps": {"fsl 6.0.4": {"version": "20210101"}}}}`
	tests := []struct {
		name        string
		existing    string // apps.json before the sync; no file when empty
		incomplete  string // apps.json.incomplete before the sync; no file when empty
		status      int    // the server's status; 200 when zero
		body        string
		gzip        bool // the server compresses the body
		short       bool // the server promises more bytes than it sends
		closed      bool // the server is gone before the request
		wantChanged bool
		want        string // apps.json afterwards
		wantErr     string
	}{
		{name: "first sync", body: catalogue, wantChanged: true, want: catalogue},
		{name: "same catalogue", existing: catalogue, body: catalogue, want: catalogue},
		{name: "different catalogue", existing: catalogue, body: other, wantChanged: true, want: other},
		{name: "damaged local copy is replaced", existing: "{}", body: catalogue, wantChanged: true,
			want: catalogue},
		{name: "stale incomplete file", incomplete: strings.Repeat("x", 500), body: catalogue,
			wantChanged: true, want: catalogue},
		{name: "compressed", body: catalogue, gzip: true, wantChanged: true, want: catalogue},
		{name: "not found", existing: catalogue, status: http.StatusNotFound, want: catalogue,
			wantErr: "returned 404 Not Found"},
		{name: "server error", existing: catalogue, status: http.StatusInternalServerError, want: catalogue,
			wantErr: "returned 500 Internal Server Error"},
		{name: "not modified", existing: catalogue, status: http.StatusNotModified, want: catalogue,
			wantErr: "returned 304 Not Modified"},
		{name: "invalid JSON", existing: catalogue, body: "{", want: catalogue,
			wantErr: "downloaded catalogue is invalid: unexpected end of JSON input"},
		{name: "no containers", existing: catalogue, body: "{}", want: catalogue,
			wantErr: "downloaded catalogue is invalid: no containers"},
		{name: "body cut short", existing: catalogue, body: catalogue, short: true, want: catalogue,
			wantErr: "downloading catalogue: unexpected EOF"},
		{name: "server gone", existing: catalogue, closed: true, want: catalogue,
			wantErr: "downloading catalogue: Get "},
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
			if tt.incomplete != "" {
				if err := os.WriteFile(path+".incomplete", []byte(tt.incomplete), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			catalogueURL := srv.URL + "/apps.json"
			changed, err := syncCatalogue(srv.Client(), catalogueURL, dir)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				if tt.status != 0 && (err == nil || !strings.Contains(err.Error(), catalogueURL)) {
					t.Errorf("error = %v, want it to name %s", err, catalogueURL)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tt.wantChanged)
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
			if tt.wantChanged {
				if _, err := os.Stat(path + ".incomplete"); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("apps.json.incomplete left behind: %v", err)
				}

				// A second sync must see what the first wrote as the same catalogue.
				if err := os.Chtimes(path, old, old); err != nil {
					t.Fatal(err)
				}
				changed, err := syncCatalogue(srv.Client(), catalogueURL, dir)
				if err != nil || changed {
					t.Errorf("second sync: changed = %v, error = %v, want no change", changed, err)
				}
				if info, err := os.Stat(path); err != nil || !info.ModTime().Equal(old) {
					t.Errorf("second sync rewrote apps.json: %v", err)
				}
			}
			wantRequests := 1
			switch {
			case tt.closed:
				wantRequests = 0
			case tt.wantChanged:
				wantRequests = 2 // the second sync above
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
