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

func TestLoadCatalogList(t *testing.T) {
	const a = `{"name": "neuroimaging", "url": "https://example.org/neuroimaging/apps.json", "use": true}`
	const b = `{"name": "mylab", "url": "https://lab.example/neuroimaging/apps.json", "use": false}`
	tests := []struct {
		name    string
		json    string // written to catalogs.json; no file when empty
		dir     bool   // create catalogs.json as a directory to force a read fail
		want    []catalog
		wantErr string
	}{
		{name: "missing file"},
		{name: "empty list", json: "[]", want: []catalog{}},
		{name: "in order", json: "[" + a + ", " + b + "]", want: []catalog{
			{Name: "neuroimaging", URL: "https://example.org/neuroimaging/apps.json", Use: true},
			{Name: "mylab", URL: "https://lab.example/neuroimaging/apps.json"}}},
		{name: "same URL under two names",
			json: "[" + a + `, {"name": "neuro", "url": "https://example.org/neuroimaging/apps.json", "use": true}]`,
			want: []catalog{
				{Name: "neuroimaging", URL: "https://example.org/neuroimaging/apps.json", Use: true},
				{Name: "neuro", URL: "https://example.org/neuroimaging/apps.json", Use: true}}},
		{name: "share", json: `[{"name":"a","url":"https://e.org/a.json","use":true,"dir":"/share","layout":"{tool}/{version}/{date}"}]`,
			want: []catalog{{Name: "a", URL: "https://e.org/a.json", Use: true, Dir: "/share", Layout: "{tool}/{version}/{date}"}}},
		{name: "relative share", json: `[{"name":"a","url":"https://e.org/a.json","use":true,"dir":"share"}]`, wantErr: "invalid dir"},
		{name: "bad layout", json: `[{"name":"a","url":"https://e.org/a.json","use":true,"dir":"/share","layout":"{name}"}]`, wantErr: "invalid layout"},
		{name: "layout without dir", json: `[{"name":"a","url":"https://e.org/a.json","use":true,"layout":"{tool}/{version}/{date}"}]`, wantErr: "layout without a dir"},
		{name: "dir case", json: `[{"name":"a","url":"https://e.org/a.json","use":true,"Dir":"/share"}]`, wantErr: `unknown field "Dir"`},
		{name: "layout case", json: `[{"name":"a","url":"https://e.org/a.json","use":true,"Layout":"{tool}/{version}/{date}"}]`, wantErr: `unknown field "Layout"`},
		{name: "path is a directory", dir: true, wantErr: "reading catalogs: "},
		{name: "invalid JSON", json: "[", wantErr: "unexpected EOF"},
		{name: "anything after the list", json: "[] []", wantErr: "unexpected data after the list"},
		{name: "top-level null", json: "null", wantErr: "not a JSON array"},
		{name: "object", json: "{}", wantErr: "cannot unmarshal object"},
		{name: "unknown field", json: `[{"name": "a", "url": "https://e.org/a.json", "use": true, "x": 1}]`,
			wantErr: `unknown field "x"`},
		{name: "field names match exactly", json: `[{"Name": "a", "url": "https://e.org/a.json", "USE": true}]`,
			wantErr: `unknown field "Name"`},
		{name: "use missing", json: `[{"name": "a", "url": "https://e.org/a.json"}]`,
			wantErr: `catalog "a" has no "use"`},
		{name: "use null", json: `[{"name": "a", "url": "https://e.org/a.json", "use": null}]`,
			wantErr: `catalog "a" has no "use"`},
		{name: "use as a string", json: `[{"name": "a", "url": "https://e.org/a.json", "use": "true"}]`,
			wantErr: "cannot unmarshal string"},
		{name: "name missing", json: `[{"url": "https://e.org/a.json", "use": true}]`,
			wantErr: `invalid catalog name ""`},
		{name: "no url: switches a system catalog", json: `[{"name": "a", "use": false}]`,
			want: []catalog{{Name: "a"}}},
		{name: "no url with a dir", json: `[{"name": "a", "use": true, "dir": "/d"}]`,
			wantErr: `catalog "a" has a dir or layout but no url`},
		{name: "url empty", json: `[{"name": "a", "url": "", "use": true}]`,
			want: []catalog{{Name: "a", Use: true}}},
		{name: "bad name", json: `[{"name": "My Lab", "url": "https://e.org/a.json", "use": true}]`,
			wantErr: `invalid catalog name "My Lab"`},
		{name: "credentials in the URL", json: `[{"name": "a", "url": "https://u:p@e.org/a.json", "use": true}]`,
			wantErr: `catalog "a" has an invalid URL: must not contain credentials`},
		{name: "name listed twice", json: "[" + a + ", " + a + "]",
			wantErr: `catalog "neuroimaging" is listed twice`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "catalogs.json")
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
			got, err := loadCatalogList(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				// os errors are *fs.PathError, which already include the path.
				prefix := "reading catalogs " + path + ": "
				if tt.dir {
					prefix = "reading catalogs: "
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
				t.Errorf("catalogs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWriteCatalogList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sciget", "catalogs.json")
	tests := []struct {
		name     string
		catalogs []catalog
		want     string
	}{
		{name: "none is an empty list", want: "[]\n"},
		{name: "one", catalogs: []catalog{{Name: "a", URL: "https://e.org/a.json", Use: true}},
			want: "[\n  {\n    \"name\": \"a\",\n    \"url\": \"https://e.org/a.json\",\n    \"use\": true\n  }\n]\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := writeCatalogList(path, tt.catalogs, false); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tt.want {
				t.Errorf("file = %q, want %q", data, tt.want)
			}
			if _, err := loadCatalogList(path); err != nil {
				t.Errorf("written file does not load: %v", err)
			}
		})
	}
}

func TestCheckCatalogName(t *testing.T) {
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
			if err := checkCatalogName(tt.value); (err != nil) != tt.wantErr {
				t.Errorf("checkCatalogName(%q) = %v, want error: %v", tt.value, err, tt.wantErr)
			}
		})
	}
}

func TestCheckCatalogURL(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "https", value: "https://example.org/neuroimaging/apps.json"},
		{name: "http refused", value: "http://example.org/apps.json",
			wantErr: "must use https; plain http is allowed only to this machine (localhost)"},
		{name: "http to a loopback address", value: "http://127.0.0.1:8080/apps.json"},
		{name: "http to localhost", value: "http://localhost:8080/apps.json"},
		{name: "http to IPv6 loopback", value: "http://[::1]:8080/apps.json"},
		{name: "http to a name like localhost", value: "http://localhost.example.org/apps.json",
			wantErr: "must use https; plain http is allowed only to this machine (localhost)"},
		{name: "no scheme", value: "example.org/apps.json", wantErr: "must start with https:// or http://"},
		{name: "another scheme", value: "ftp://example.org/apps.json", wantErr: "must start with https:// or http://"},
		{name: "no host", value: "https:///apps.json", wantErr: "must name a host"},
		{name: "a port but no host", value: "http://:80/apps.json", wantErr: "must name a host"},
		{name: "user and password", value: "https://u:p@example.org/apps.json", wantErr: "must not contain credentials"},
		{name: "user only", value: "https://u@example.org/apps.json", wantErr: "must not contain credentials"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCatalogURL(tt.value)
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

func TestCatalogDir(t *testing.T) {
	tests := []struct {
		name string
		c    catalog
		want string
	}{
		{name: "name and URL hash", c: catalog{Name: "neuroimaging", URL: "https://example.org/neuroimaging/apps.json"},
			want: "/c/catalogs/neuroimaging-77a7c3e239d9"},
		{name: "same name, another URL", c: catalog{Name: "neuroimaging", URL: "https://mirror.example.org/neuroimaging/apps.json"},
			want: "/c/catalogs/neuroimaging-b5791028df25"},
		{name: "same URL, another name", c: catalog{Name: "neuro", URL: "https://example.org/neuroimaging/apps.json"},
			want: "/c/catalogs/neuro-77a7c3e239d9"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.dir("/c"); got != filepath.FromSlash(tt.want) {
				t.Errorf("dir = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteCatalogTable(t *testing.T) {
	catalogs := []catalog{
		{Name: "mylab", URL: "https://lab.example/a.json", Use: true, Dir: "/share"},
		{Name: "old", URL: "https://old.example/a.json"},
		{Name: "neuroimaging", URL: "https://example.org/a.json", Use: true, System: true},
	}
	tests := []struct {
		name     string
		terminal bool
		want     string
	}{
		{name: "tab-separated when piped",
			want: "mylab\t1\tuser\thttps://lab.example/a.json\t/share\nold\t-\tuser\thttps://old.example/a.json\t\n" +
				"neuroimaging\t2\tsystem\thttps://example.org/a.json\t\n"},
		{name: "aligned with a header on a terminal", terminal: true,
			want: fmt.Sprintf("%-14s%-7s%-8s%-28s%s\n", "NAME", "ORDER", "SOURCE", "URL", "DIR") +
				fmt.Sprintf("%-14s%-7s%-8s%-28s%s\n", "mylab", "1", "user", "https://lab.example/a.json", "/share") +
				fmt.Sprintf("%-14s%-7s%-8s%-28s%s\n", "old", "-", "user", "https://old.example/a.json", "") +
				fmt.Sprintf("%-14s%-7s%-8s%-28s%s\n", "neuroimaging", "2", "system", "https://example.org/a.json", "")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeCatalogTable(&out, catalogs, tt.terminal); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("output =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestReadSystemCatalogs(t *testing.T) {
	tests := []struct {
		name    string
		json    string // the system catalogs.json; none when empty
		want    []catalog
		wantErr string
	}{
		{name: "none"},
		{name: "marked as system", json: `[{"name": "a", "url": "https://e.org/a.json", "use": true}]`,
			want: []catalog{{Name: "a", URL: "https://e.org/a.json", Use: true, System: true}}},
		{name: "an entry without a url", json: `[{"name": "a", "use": true}]`, wantErr: `catalog "a" has no url`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			setSystemDir(t, dir)
			if tt.json != "" {
				if err := os.WriteFile(filepath.Join(dir, "catalogs.json"), []byte(tt.json), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, got, err := readSystemCatalogs()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("catalogs = %v, want %v", got, tt.want)
			}
		})
	}
}
