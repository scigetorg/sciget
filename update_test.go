package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLatestRelease(t *testing.T) {
	for _, tt := range []struct {
		name    string
		to      string // where /releases/latest redirects; it answers 404 when empty
		want    string
		wantErr string
	}{
		{name: "tag", to: "/scigetorg/sciget/releases/tag/v0.2.0", want: "v0.2.0"},
		{name: "through a renamed repository", to: "/moved/releases/latest", want: "v0.3.0"},
		{name: "escape sequence removed", to: "/x/releases/tag/v0.2.0%1B%5B2J", want: "v0.2.0"},
		{name: "not found", wantErr: "returned 404 Not Found, not a release"},
		{name: "no releases", to: "/scigetorg/sciget/releases", wantErr: "checking for updates: no releases yet"},
		{name: "a page, not a redirect", to: "/page", wantErr: "returned 200 OK, not a release"},
		{name: "no tag", to: "/x/releases/tag/", wantErr: "the release has no tag"},
		{name: "redirect to http refused", to: "http://example.org/latest", wantErr: "refused a redirect to http://example.org/latest"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("User-Agent") != "sciget/"+version {
					t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
				}
				switch {
				case strings.Contains(r.URL.Path, "/releases/tag/"):
					t.Errorf("fetched the tag's page %s", r.URL.Path)
				case r.URL.Path == "/moved/releases/latest":
					http.Redirect(w, r, "/moved/releases/tag/v0.3.0", http.StatusFound)
				case r.URL.Path == "/scigetorg/sciget/releases":
					t.Errorf("fetched the releases page")
				case r.URL.Path == "/page":
					w.Write([]byte("<html>"))
				case tt.to != "":
					http.Redirect(w, r, tt.to, http.StatusFound)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			got, err := latestRelease(srv.Client(), srv.URL+"/scigetorg/sciget/releases/latest")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("latestRelease() = %q, %v, want %q", got, err, tt.want)
			}
		})
	}
}

func TestUpdateResult(t *testing.T) {
	for _, tt := range []struct {
		name, running, latest, want string
	}{
		{name: "newer", running: "v0.1.0", latest: "v0.2.0", want: "↑ v0.2.0 available · see github.com/scigetorg/sciget/releases"},
		{name: "newer patch", running: "v0.1.9", latest: "v0.1.10", want: "↑ v0.1.10 available · see github.com/scigetorg/sciget/releases"},
		{name: "same", running: "v0.1.0", latest: "v0.1.0", want: "√ up to date"},
		{name: "older", running: "v0.3.0", latest: "v0.2.9", want: "√ up to date"},
		{name: "no v prefix", running: "0.1.0", latest: "v0.1.0", want: "√ up to date"},
		{name: "development build", running: "dev", latest: "v0.2.0", want: "development build"},
		{name: "unparsable running", running: "abc", latest: "v0.2.0", want: "unknown version abc"},
		{name: "unparsable latest", running: "v0.1.0", latest: "latest", want: "couldn't read the latest version"},
		{name: "a release candidate is older", running: "v0.1.0-rc1", latest: "v0.1.0", want: "↑ v0.1.0 available · see github.com/scigetorg/sciget/releases"},
		{name: "running a newer candidate", running: "v0.2.0-rc1", latest: "v0.1.0", want: "√ up to date"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := updateResult(tt.running, tt.latest); got != tt.want {
				t.Errorf("updateResult(%q, %q) = %q, want %q", tt.running, tt.latest, got, tt.want)
			}
		})
	}
}
