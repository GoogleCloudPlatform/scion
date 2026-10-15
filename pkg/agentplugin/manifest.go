// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agentplugin

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
)

// Manifest is a validated Agent Plugins 1.0.0 plugin.json.
type Manifest struct {
	Schema      string
	Name        string
	Version     string
	Description string
	Homepage    string
	Repository  string
	License     string
	Author      *Author
	Keywords    []string
	// Extensions is kept raw and never interpreted (spec §8.1). It is nil
	// when absent or ignored because it is not an object.
	Extensions json.RawMessage
}

// Author is the closed author object of a manifest.
type Author struct {
	Name  string
	Email string
	URL   string
}

// UnsupportedSchemaError reports a plugin.json whose $schema targets an
// Agent Plugins version Scion does not support.
type UnsupportedSchemaError struct {
	Name   string // manifest name when it is a string, else ""
	Schema string
}

func (e *UnsupportedSchemaError) Error() string {
	msg := fmt.Sprintf("unsupported Agent Plugins version %q; Scion supports %s", e.Schema, SpecVersion)
	if e.Name != "" {
		return fmt.Sprintf("plugin %q: %s", e.Name, msg)
	}
	return msg
}

// LoadManifest reads plugin.json through root (Lstat: must be a regular file,
// never a symlink). Unknown top-level fields are reported as warnings
// (spec §5.2) and ignored; every permitted field is type-checked strictly;
// author is closed; a non-object extensions is a warning and ignored (§8.1).
//
// A fatal problem is returned as an error: a missing or non-regular file, a
// file over 1 MiB, invalid JSON or a non-object, a $schema other than
// PluginSchemaV1 (*UnsupportedSchemaError), an invalid name, or any type
// violation. Diagnostics carry no Plugin field; the caller sets it.
func LoadManifest(root *os.Root) (*Manifest, []Diagnostic, error) {
	data, err := readRegularFile(root, ManifestFile, maxJSONFileSize)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("%s not found", ManifestFile)
		}
		return nil, nil, err
	}
	members, err := decodeObject(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %v", ManifestFile, err)
	}

	m := &Manifest{}
	rawSchema, ok := members["$schema"]
	if !ok {
		return nil, nil, fmt.Errorf("%s: missing required field \"$schema\"", ManifestFile)
	}
	schema, ok := jsonString(rawSchema)
	if !ok {
		return nil, nil, fmt.Errorf("%s: field \"$schema\" must be a string", ManifestFile)
	}
	if schema != PluginSchemaV1 {
		name, _ := jsonString(members["name"])
		if ValidateName(name) != nil {
			name = ""
		}
		return nil, nil, &UnsupportedSchemaError{Name: name, Schema: schema}
	}
	m.Schema = schema

	rawName, ok := members["name"]
	if !ok {
		return nil, nil, fmt.Errorf("%s: missing required field \"name\"", ManifestFile)
	}
	if m.Name, ok = jsonString(rawName); !ok {
		return nil, nil, fmt.Errorf("%s: field \"name\" must be a string", ManifestFile)
	}
	if err := ValidateName(m.Name); err != nil {
		return nil, nil, fmt.Errorf("%s: invalid name: %v", ManifestFile, err)
	}

	var diags []Diagnostic
	keys := make([]string, 0, len(members))
	for k := range members {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		raw := members[k]
		switch k {
		case "$schema", "name":
		case "version", "description", "homepage", "repository", "license":
			s, ok := jsonString(raw)
			if !ok {
				return nil, nil, fmt.Errorf("%s: field %q must be a string", ManifestFile, k)
			}
			switch k {
			case "version":
				m.Version = s
			case "description":
				m.Description = s
			case "homepage":
				m.Homepage = s
			case "repository":
				m.Repository = s
			case "license":
				m.License = s
			}
		case "author":
			a, err := decodeAuthor(raw)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: field \"author\": %v", ManifestFile, err)
			}
			m.Author = a
		case "keywords":
			kw, ok := jsonStringArray(raw)
			if !ok {
				return nil, nil, fmt.Errorf("%s: field \"keywords\" must be an array of strings", ManifestFile)
			}
			m.Keywords = kw
		case "extensions":
			if !isJSONKind(raw, '{') {
				diags = append(diags, warning("manifest field \"extensions\" is not an object; ignored"))
				continue
			}
			m.Extensions = append(json.RawMessage(nil), raw...)
		default:
			diags = append(diags, warning("unknown manifest field %q ignored", k))
		}
	}
	return m, diags, nil
}

func decodeAuthor(raw json.RawMessage) (*Author, error) {
	if !isJSONKind(raw, '{') {
		return nil, fmt.Errorf("must be an object")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, fmt.Errorf("must be an object")
	}
	a := &Author{}
	keys := make([]string, 0, len(members))
	for k := range members {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		s, ok := jsonString(members[k])
		switch k {
		case "name", "email", "url":
			if !ok {
				return nil, fmt.Errorf("%q must be a string", k)
			}
		default:
			return nil, fmt.Errorf("unknown field %q", k)
		}
		switch k {
		case "name":
			a.Name = s
		case "email":
			a.Email = s
		case "url":
			a.URL = s
		}
	}
	return a, nil
}
