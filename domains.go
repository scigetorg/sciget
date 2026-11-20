package main

import (
	"encoding/json"
	"errors"
	"fmt"
)

// isDomainsFile reports whether data is a domains file: several catalog
// JSONs under a top-level "domains" key.
func isDomainsFile(data []byte) bool {
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return false
	}
	_, ok := top["domains"]
	return ok
}

// splitDomains returns the catalog JSON of each key in a domains file, with
// the file's schemaVersion added. It checks the file, not the values or
// keys: each catalog checks its own.
func splitDomains(data []byte) (map[string][]byte, error) {
	var file struct {
		SchemaVersion int                        `json:"schemaVersion"`
		Domains       map[string]json.RawMessage `json:"domains"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	switch {
	case file.SchemaVersion > 1:
		return nil, fmt.Errorf("schemaVersion %d needs a newer sciget", file.SchemaVersion)
	case file.SchemaVersion != 1:
		return nil, errors.New("schemaVersion must be 1")
	case len(file.Domains) == 0:
		return nil, errors.New("no domains")
	}
	catalogs := map[string][]byte{}
	for key, value := range file.Domains {
		var fields map[string]json.RawMessage
		if json.Unmarshal(value, &fields) != nil || fields == nil {
			catalogs[key] = value // not an object, so its own check fails
			continue
		}
		fields["schemaVersion"] = json.RawMessage("1")
		data, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		catalogs[key] = data
	}
	return catalogs, nil
}
