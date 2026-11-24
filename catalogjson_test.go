package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseCatalogJSON(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		want    catalogJSON
		wantErr string
	}{
		{name: "full", json: `{"schemaVersion": 1, "title": "Neuro", "description": "d", "appJsonUrl": "apps.json",
			"downloadUrls": ["https://a.example/{file}", "http://localhost:8080/{file}"]}`,
			want: catalogJSON{SchemaVersion: 1, Title: "Neuro", Description: "d", AppJSONURL: "apps.json",
				DownloadURLs: []string{"https://a.example/{file}", "http://localhost:8080/{file}"}}},
		{name: "other keys ignored", json: `{"schemaVersion": 1, "appJsonUrl": "a", "downloadUrls": ["https://a.example/{file}"], "registry": "x"}`,
			want: catalogJSON{SchemaVersion: 1, AppJSONURL: "a", DownloadURLs: []string{"https://a.example/{file}"}}},
		{name: "no schemaVersion", json: `{"appJsonUrl": "a", "downloadUrls": ["https://a.example/{file}"]}`,
			wantErr: "schemaVersion must be 1"},
		{name: "newer schemaVersion", json: `{"schemaVersion": 2}`, wantErr: "schemaVersion 2 needs a newer sciget"},
		{name: "an empty catalog", json: `{"schemaVersion": 1, "title": "Soon"}`,
			want: catalogJSON{SchemaVersion: 1, Title: "Soon"}},
		{name: "an empty appJsonUrl is an empty catalog", json: `{"schemaVersion": 1, "appJsonUrl": ""}`,
			want: catalogJSON{SchemaVersion: 1}},
		{name: "a null appJsonUrl is an empty catalog", json: `{"schemaVersion": 1, "appJsonUrl": null}`,
			want: catalogJSON{SchemaVersion: 1}},
		{name: "an empty catalog with mirrors", json: `{"schemaVersion": 1, "downloadUrls": ["https://a.example/{file}"]}`,
			want: catalogJSON{SchemaVersion: 1, DownloadURLs: []string{"https://a.example/{file}"}}},
		{name: "an empty catalog with a bad mirror", json: `{"schemaVersion": 1, "downloadUrls": ["https://a.example/"]}`,
			wantErr: "must contain {file} exactly once"},
		{name: "a share-only catalog", json: `{"schemaVersion": 1, "appJsonUrl": "a"}`,
			want: catalogJSON{SchemaVersion: 1, AppJSONURL: "a"}},
		{name: "empty downloadUrls", json: `{"schemaVersion": 1, "appJsonUrl": "a", "downloadUrls": []}`,
			want: catalogJSON{SchemaVersion: 1, AppJSONURL: "a", DownloadURLs: []string{}}},
		{name: "{file} twice", json: `{"schemaVersion": 1, "appJsonUrl": "a", "downloadUrls": ["https://a.example/{file}/{file}"]}`,
			wantErr: `download URL "https://a.example/{file}/{file}" must contain {file} exactly once`},
		{name: "plain http to another machine", json: `{"schemaVersion": 1, "appJsonUrl": "a", "downloadUrls": ["http://a.example/{file}"]}`,
			wantErr: "must use https"},
		{name: "not an object", json: `[]`, wantErr: "cannot unmarshal array"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCatalogJSON([]byte(tt.json))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.SchemaVersion != tt.want.SchemaVersion || got.Title != tt.want.Title || got.Description != tt.want.Description ||
				got.AppJSONURL != tt.want.AppJSONURL || !slices.Equal(got.DownloadURLs, tt.want.DownloadURLs) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolveLink(t *testing.T) {
	local := filepath.Join(t.TempDir(), "cat", "catalog.json")
	tests := []struct {
		name    string
		base    string
		ref     string
		want    string
		wantErr string
	}{
		{name: "relative to a URL", base: "https://a.example/neuro/catalog.json", ref: "apps.json",
			want: "https://a.example/neuro/apps.json"},
		{name: "up from a URL", base: "https://a.example/neuro/catalog.json", ref: "../microscopy/apps.json",
			want: "https://a.example/microscopy/apps.json"},
		{name: "rooted on a URL's host", base: "https://a.example/neuro/catalog.json", ref: "/apps.json",
			want: "https://a.example/apps.json"},
		{name: "absolute from a URL", base: "https://a.example/catalog.json", ref: "https://b.example/apps.json",
			want: "https://b.example/apps.json"},
		{name: "relative to a local file", base: local, ref: "apps.json", want: filepath.Join(filepath.Dir(local), "apps.json")},
		{name: "absolute URL from a local file", base: local, ref: "https://b.example/apps.json",
			want: "https://b.example/apps.json"},
		{name: "absolute path from a local file", base: local, ref: filepath.Join(filepath.Dir(local), "x.json"),
			want: filepath.Join(filepath.Dir(local), "x.json")},
		{name: "plain http from a URL", base: "https://a.example/catalog.json", ref: "http://b.example/apps.json",
			wantErr: "must use https"},
		{name: "credentials", base: "https://a.example/catalog.json", ref: "https://u:p@b.example/apps.json",
			wantErr: "must not contain"},
		{name: "a drive path from a URL stays a URL", base: "https://a.example/catalog.json", ref: "C:/tmp/apps.json",
			wantErr: "must start with https://"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveLink(tt.base, tt.ref)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadLink(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "small.json")
	large := filepath.Join(dir, "large.json")
	if err := os.WriteFile(small, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(large, make([]byte, maxSize+1), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest.json" {
			http.Redirect(w, r, "/release/catalog.json", http.StatusFound)
			return
		}
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	tests := []struct {
		name     string
		link     string
		wantBase string
		wantErr  string
	}{
		{name: "local file", link: small, wantBase: small},
		{name: "local file over the cap", link: large, wantErr: "is larger than max_catalog_size (1024 bytes)"},
		{name: "missing local file", link: filepath.Join(dir, "none.json"), wantErr: "reading catalog: open "},
		{name: "URL", link: srv.URL + "/catalog.json", wantBase: srv.URL + "/catalog.json"},
		{name: "base is where a redirect ends", link: srv.URL + "/latest.json", wantBase: srv.URL + "/release/catalog.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, base, err := readLink(srv.Client(), tt.link, maxSize)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != "{}" || base != tt.wantBase {
				t.Errorf("got %q, %q, want %q, %q", data, base, "{}", tt.wantBase)
			}
		})
	}
}

func TestOnceReader(t *testing.T) {
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets++
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	read := onceReader(srv.Client(), maxSize)
	for range 3 {
		if _, _, err := read(srv.URL + "/domains.json"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := read(srv.URL + "/apps.json"); err != nil {
		t.Fatal(err)
	}
	if gets != 2 {
		t.Errorf("server saw %d requests, want 2", gets)
	}
}
