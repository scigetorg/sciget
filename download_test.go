package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDownloadFile(t *testing.T) {
	const body = "a container image"
	sum := sha256.Sum256([]byte(body))
	goodHash := hex.EncodeToString(sum[:])
	tests := []struct {
		name      string
		headSize  int    // the size HEAD reports; len(body) when zero
		headless  bool   // HEAD gives no Content-Length
		status    int    // the status of both requests; 200 when zero
		get       string // the GET body; body when empty
		short     bool   // the GET promises len(body) bytes but sends fewer
		want      download
		wantErr   string
		wantCalls bool // progress was called with the full size
	}{
		{name: "download", wantCalls: true},
		{name: "matching SHA-256", want: download{size: int64(len(body)), sha256: goodHash}, wantCalls: true},
		{name: "wrong SHA-256", want: download{sha256: strings.Repeat("0", 64)}, wantErr: "SHA-256 does not match"},
		{name: "size differs from the catalog", want: download{size: 5}, wantErr: "but the catalog says 5"},
		{name: "not found", status: http.StatusNotFound, wantErr: "404"},
		{name: "no size", headless: true, wantErr: "no size given"},
		{name: "short body", short: true, wantErr: "unexpected EOF"},
		{name: "body longer than HEAD said", get: body + body, wantErr: "expected 17"},
		{name: "body shorter than HEAD said", headSize: 100, wantErr: "got 17 bytes, expected 100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.status != 0 {
					w.WriteHeader(tt.status)
					return
				}
				if r.Method == http.MethodHead {
					size := tt.headSize
					if size == 0 {
						size = len(body)
					}
					if !tt.headless {
						w.Header().Set("Content-Length", strconv.Itoa(size))
					}
					return
				}
				get := tt.get
				if get == "" {
					get = body
				}
				if tt.short {
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
					get = body[:5]
				} else {
					// No Content-Length, so the body ends only when the
					// server stops sending.
					w.Header().Set("Transfer-Encoding", "chunked")
				}
				w.Write([]byte(get))
			}))
			defer srv.Close()

			dir := t.TempDir()
			dest := filepath.Join(dir, "fsl.simg")
			// Files a download must leave alone.
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dest+".1.incomplete", []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, dest+".2.incomplete"); err != nil {
				t.Fatal(err)
			}

			var calls [][2]int64
			path, err := downloadFile(context.Background(), srv.Client(), srv.URL+"/fsl.simg", dest, tt.want, func(done, total int64) {
				calls = append(calls, [2]int64{done, total})
			})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}

			matches, _ := filepath.Glob(dest + ".*.incomplete")
			wantFiles := 2
			if err == nil {
				wantFiles = 3
				got, readErr := os.ReadFile(path)
				if readErr != nil || string(got) != body {
					t.Errorf("downloaded %q, %v; want %q", got, readErr, body)
				}
			}
			if len(matches) != wantFiles {
				t.Errorf("incomplete files = %q, want %d (a failed download deletes its own)", matches, wantFiles)
			}
			if got, _ := os.ReadFile(outside); string(got) != "keep" {
				t.Errorf("the symlink's target was changed to %q", got)
			}
			if got, _ := os.ReadFile(dest + ".1.incomplete"); string(got) != "old" {
				t.Errorf("an earlier incomplete file was changed to %q", got)
			}
			last := len(calls) - 1
			if tt.wantCalls && (last < 0 || calls[last] != [2]int64{int64(len(body)), int64(len(body))}) {
				t.Errorf("progress calls = %v, want the last to be the full size", calls)
			}
		})
	}
}

func TestCheckSpace(t *testing.T) {
	tests := []struct {
		name    string
		free    int64
		need    int64
		wantErr string
	}{
		{name: "enough", free: 10, need: 10},
		{name: "unknown", free: -1, need: 10},
		{name: "too little", free: 3_100_000_000, need: 8_800_000_000, wantErr: "not enough space in /store: needs 8.8 GB, 3.1 GB free"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkSpace(tt.free, tt.need, "/store")
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr) {
				t.Errorf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{n: 512, want: "512 B"},
		{n: 1_500, want: "1.5 kB"},
		{n: 97_243_136, want: "97.2 MB"},
		{n: 8_760_000_000, want: "8.8 GB"},
	}
	for _, tt := range tests {
		if got := humanSize(tt.n); got != tt.want {
			t.Errorf("humanSize(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestNewDownloadClient(t *testing.T) {
	cfg := config{overrides: map[string]string{}}
	client, err := newDownloadClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if client.Timeout != 0 {
		t.Errorf("Timeout = %v, want none", client.Timeout)
	}
	transport := client.Transport.(*http.Transport)
	if transport.ResponseHeaderTimeout == 0 || !transport.DisableCompression {
		t.Errorf("ResponseHeaderTimeout = %v, DisableCompression = %v", transport.ResponseHeaderTimeout, transport.DisableCompression)
	}
}
