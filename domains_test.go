package main

import (
	"maps"
	"strings"
	"testing"
)

func TestSplitDomains(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		want    map[string]string
		wantErr string
	}{
		{name: "two keys, schemaVersion added",
			json: `{"schemaVersion": 1, "domains": {"neuro": {"appJsonUrl": "n.json"}, "eeg": {"appJsonUrl": "e.json", "schemaVersion": 7}}}`,
			want: map[string]string{"neuro": `{"appJsonUrl":"n.json","schemaVersion":1}`, "eeg": `{"appJsonUrl":"e.json","schemaVersion":1}`}},
		{name: "a value that is not an object is passed on", json: `{"schemaVersion": 1, "domains": {"a": 5, "b": {}}}`,
			want: map[string]string{"a": "5", "b": `{"schemaVersion":1}`}},
		{name: "keys not checked", json: `{"schemaVersion": 1, "domains": {"Bad Key": {}}}`,
			want: map[string]string{"Bad Key": `{"schemaVersion":1}`}},
		{name: "other top-level keys ignored", json: `{"schemaVersion": 1, "x": 1, "domains": {"a": {}}}`,
			want: map[string]string{"a": `{"schemaVersion":1}`}},
		{name: "empty domains", json: `{"schemaVersion": 1, "domains": {}}`, wantErr: "no domains"},
		{name: "no schemaVersion", json: `{"domains": {"a": {}}}`, wantErr: "schemaVersion must be 1"},
		{name: "newer schemaVersion", json: `{"schemaVersion": 2, "domains": {"a": {}}}`,
			wantErr: "schemaVersion 2 needs a newer sciget"},
		{name: "domains not an object", json: `{"schemaVersion": 1, "domains": []}`, wantErr: "cannot unmarshal array"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitDomains([]byte(tt.json))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			gotText := map[string]string{}
			for k, v := range got {
				gotText[k] = string(v)
			}
			if !maps.Equal(gotText, tt.want) {
				t.Errorf("got %q, want %q", gotText, tt.want)
			}
		})
	}
}

func TestIsDomainsFile(t *testing.T) {
	tests := []struct {
		json string
		want bool
	}{
		{json: `{"domains": {}}`, want: true},
		{json: `{"schemaVersion": 1, "appJsonUrl": "a"}`},
		{json: `[]`},
		{json: `{`},
	}
	for _, tt := range tests {
		t.Run(tt.json, func(t *testing.T) {
			if got := isDomainsFile([]byte(tt.json)); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
