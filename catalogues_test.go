package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadCatalogueList(t *testing.T) {
	const a = `{"name": "neuroimaging", "url": "https://example.org/neuroimaging/apps.json", "use": true}`
	const b = `{"name": "mylab", "url": "https://lab.example/neuroimaging/apps.json", "use": false}`
	tests := []struct {
		name    string
		json    string // written to catalogues.json; no file when empty
		dir     bool   // catalogues.json is a directory
		want    []catalogue
		wantErr string
	}{
		{name: "missing file"},
		{name: "empty list", json: "[]", want: []catalogue{}},
		{name: "in order", json: "[" + a + ", " + b + "]", want: []catalogue{
			{Name: "neuroimaging", URL: "https://example.org/neuroimaging/apps.json", Use: true},
			{Name: "mylab", URL: "https://lab.example/neuroimaging/apps.json"}}},
		{name: "same URL under two names",
			json: "[" + a + `, {"name": "neuro", "url": "https://example.org/neuroimaging/apps.json", "use": true}]`,
			want: []catalogue{
				{Name: "neuroimaging", URL: "https://example.org/neuroimaging/apps.json", Use: true},
				{Name: "neuro", URL: "https://example.org/neuroimaging/apps.json", Use: true}}},
		{name: "share", json: `[{"name":"a","url":"https://e.org/a.json","use":true,"dir":"/share","layout":"{tool}/{version}/{date}"}]`,
			want: []catalogue{{Name: "a", URL: "https://e.org/a.json", Use: true, Dir: "/share", Layout: "{tool}/{version}/{date}"}}},
		{name: "relative share", json: `[{"name":"a","url":"https://e.org/a.json","use":true,"dir":"share"}]`, wantErr: "invalid dir"},
		{name: "bad layout", json: `[{"name":"a","url":"https://e.org/a.json","use":true,"dir":"/share","layout":"{name}"}]`, wantErr: "invalid layout"},
		{name: "layout without dir", json: `[{"name":"a","url":"https://e.org/a.json","use":true,"layout":"{tool}/{version}/{date}"}]`, wantErr: "layout without a dir"},
		{name: "dir case", json: `[{"name":"a","url":"https://e.org/a.json","use":true,"Dir":"/share"}]`, wantErr: `unknown field "Dir"`},
		{name: "layout case", json: `[{"name":"a","url":"https://e.org/a.json","use":true,"Layout":"{tool}/{version}/{date}"}]`, wantErr: `unknown field "Layout"`},
		{name: "path is a directory", dir: true, wantErr: "reading catalogues: "},
		{name: "invalid JSON", json: "[", wantErr: "unexpected EOF"},
		{name: "anything after the list", json: "[] []", wantErr: "unexpected data after the list"},
		{name: "top-level null", json: "null", wantErr: "not a JSON array"},
		{name: "object", json: "{}", wantErr: "cannot unmarshal object"},
		{name: "unknown field", json: `[{"name": "a", "url": "https://e.org/a.json", "use": true, "x": 1}]`,
			wantErr: `unknown field "x"`},
		{name: "field names match exactly", json: `[{"Name": "a", "url": "https://e.org/a.json", "USE": true}]`,
			wantErr: `unknown field "Name"`},
		{name: "use missing", json: `[{"name": "a", "url": "https://e.org/a.json"}]`,
			wantErr: `catalogue "a" has no "use"`},
		{name: "use null", json: `[{"name": "a", "url": "https://e.org/a.json", "use": null}]`,
			wantErr: `catalogue "a" has no "use"`},
		{name: "use as a string", json: `[{"name": "a", "url": "https://e.org/a.json", "use": "true"}]`,
			wantErr: "cannot unmarshal string"},
		{name: "name missing", json: `[{"url": "https://e.org/a.json", "use": true}]`,
			wantErr: `invalid catalogue name ""`},
		{name: "url null", json: `[{"name": "a", "url": null, "use": true}]`,
			wantErr: `catalogue "a" has an invalid URL: must start with https:// or http://`},
		{name: "bad name", json: `[{"name": "My Lab", "url": "https://e.org/a.json", "use": true}]`,
			wantErr: `invalid catalogue name "My Lab"`},
		{name: "credentials in the URL", json: `[{"name": "a", "url": "https://u:p@e.org/a.json", "use": true}]`,
			wantErr: `catalogue "a" has an invalid URL: must not contain credentials`},
		{name: "name listed twice", json: "[" + a + ", " + a + "]",
			wantErr: `catalogue "neuroimaging" is listed twice`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "catalogues.json")
			if tt.json != "" {
				if err := os.WriteFile(path, []byte(tt.json), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.dir {
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			got, err := loadCatalogueList(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				// os errors are *fs.PathError, which already include the path.
				prefix := "reading catalogues " + path + ": "
				if tt.dir {
					prefix = "reading catalogues: "
				}
				if !strings.HasPrefix(err.Error(), prefix) || !strings.Contains(err.Error(), path) {
					t.Errorf("error = %q, want it to start with %q and name the file", err, prefix)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("catalogues = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWriteCatalogueList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sciget", "catalogues.json")
	tests := []struct {
		name       string
		catalogues []catalogue
		want       string
	}{
		{name: "none is an empty list", want: "[]\n"},
		{name: "one", catalogues: []catalogue{{Name: "a", URL: "https://e.org/a.json", Use: true}},
			want: "[\n  {\n    \"name\": \"a\",\n    \"url\": \"https://e.org/a.json\",\n    \"use\": true\n  }\n]\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := writeCatalogueList(path, tt.catalogues); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tt.want {
				t.Errorf("file = %q, want %q", data, tt.want)
			}
			if _, err := loadCatalogueList(path); err != nil {
				t.Errorf("written file does not load: %v", err)
			}
		})
	}
}

func TestCheckCatalogueName(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "letters", value: "neuroimaging"},
		{name: "digits, hyphen and underscore", value: "lab-2_x"},
		{name: "starts with a digit", value: "2lab"},
		{name: "empty", value: "", wantErr: true},
		{name: "upper case", value: "MyLab", wantErr: true},
		{name: "space", value: "my lab", wantErr: true},
		{name: "starts with a hyphen", value: "-lab", wantErr: true},
		{name: "starts with an underscore", value: "_lab", wantErr: true},
		{name: "dot", value: "lab.org", wantErr: true},
		{name: "slash", value: "a/b", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkCatalogueName(tt.value); (err != nil) != tt.wantErr {
				t.Errorf("checkCatalogueName(%q) = %v, want error: %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestCheckCatalogueURL(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "https", value: "https://example.org/neuroimaging/apps.json"},
		{name: "http with a port", value: "http://127.0.0.1:8080/apps.json"},
		{name: "no scheme", value: "example.org/apps.json", wantErr: "must start with https:// or http://"},
		{name: "another scheme", value: "ftp://example.org/apps.json", wantErr: "must start with https:// or http://"},
		{name: "no host", value: "https:///apps.json", wantErr: "must name a host"},
		{name: "a port but no host", value: "http://:80/apps.json", wantErr: "must name a host"},
		{name: "user and password", value: "https://u:p@example.org/apps.json", wantErr: "must not contain credentials"},
		{name: "user only", value: "https://u@example.org/apps.json", wantErr: "must not contain credentials"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCatalogueURL(tt.value)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("error = %v, want none", err)
				}
			} else if err == nil || err.Error() != tt.wantErr {
				t.Errorf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestCatalogueDir(t *testing.T) {
	tests := []struct {
		name string
		c    catalogue
		want string
	}{
		{name: "name and URL hash", c: catalogue{Name: "neuroimaging", URL: "https://example.org/neuroimaging/apps.json"},
			want: "/c/catalogues/neuroimaging-77a7c3e239d9"},
		{name: "same name, another URL", c: catalogue{Name: "neuroimaging", URL: "https://mirror.example.org/neuroimaging/apps.json"},
			want: "/c/catalogues/neuroimaging-b5791028df25"},
		{name: "same URL, another name", c: catalogue{Name: "neuro", URL: "https://example.org/neuroimaging/apps.json"},
			want: "/c/catalogues/neuro-77a7c3e239d9"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.dir("/c"); got != filepath.FromSlash(tt.want) {
				t.Errorf("dir = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteCatalogueTable(t *testing.T) {
	catalogues := []catalogue{
		{Name: "mylab", URL: "https://lab.example/a.json", Use: true, Dir: "/share"},
		{Name: "old", URL: "https://old.example/a.json"},
		{Name: "neuroimaging", URL: "https://example.org/a.json", Use: true},
	}
	tests := []struct {
		name     string
		terminal bool
		want     string
	}{
		{name: "tab-separated when piped",
			want: "mylab\t1\thttps://lab.example/a.json\t/share\nold\t-\thttps://old.example/a.json\t\n" +
				"neuroimaging\t2\thttps://example.org/a.json\t\n"},
		{name: "aligned with a header on a terminal", terminal: true,
			want: fmt.Sprintf("%-14s%-7s%-28s%s\n", "NAME", "ORDER", "URL", "DIR") +
				fmt.Sprintf("%-14s%-7s%-28s%s\n", "mylab", "1", "https://lab.example/a.json", "/share") +
				fmt.Sprintf("%-14s%-7s%-28s%s\n", "old", "-", "https://old.example/a.json", "") +
				fmt.Sprintf("%-14s%-7s%-28s%s\n", "neuroimaging", "2", "https://example.org/a.json", "")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeCatalogueTable(&out, catalogues, tt.terminal); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("output =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}
