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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// readRegularFile reads name inside root. name must be a regular file (never
// a symlink) no larger than maxSize bytes. Every parent component must be a
// real directory.
func readRegularFile(root *os.Root, name string, maxSize int64) ([]byte, error) {
	if dir := filepath.Dir(name); dir != "." {
		ok, err := realDirs(root, dir)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%s: a parent directory is missing or not a real directory", name)
		}
	}
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	if fi.Size() > maxSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, maxSize)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	ofi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(fi, ofi) {
		return nil, fmt.Errorf("%s changed while being read", name)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, maxSize)
	}
	return data, nil
}

// realDirs reports whether every component of rel inside root exists and is
// a real directory (not a symlink). rel "." is the root itself.
func realDirs(root *os.Root, rel string) (bool, error) {
	rel = filepath.Clean(rel)
	if rel == "." {
		return true, nil
	}
	cur := ""
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := root.Lstat(cur)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("stat %s: %w", cur, err)
		}
		if !fi.IsDir() {
			return false, nil
		}
	}
	return true, nil
}

// decodeObject decodes data as a JSON object into its raw members. It fails
// for any other JSON value, for trailing data and for invalid JSON.
func decodeObject(data []byte) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("not a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var m map[string]json.RawMessage
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("invalid JSON: %v", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("invalid JSON: trailing data after the object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("invalid JSON: trailing data after the object")
	}
	return m, nil
}

func isJSONKind(raw json.RawMessage, first byte) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == first
}

// jsonString decodes raw as a JSON string (null and other kinds fail).
func jsonString(raw json.RawMessage) (string, bool) {
	if !isJSONKind(raw, '"') {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// jsonStringArray decodes raw as a JSON array of strings.
func jsonStringArray(raw json.RawMessage) ([]string, bool) {
	if !isJSONKind(raw, '[') {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := jsonString(it)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// jsonStringMap decodes raw as a JSON object whose values are all strings.
func jsonStringMap(raw json.RawMessage) (map[string]string, bool) {
	if !isJSONKind(raw, '{') {
		return nil, false
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, false
	}
	out := make(map[string]string, len(members))
	for k, v := range members {
		s, ok := jsonString(v)
		if !ok {
			return nil, false
		}
		out[k] = s
	}
	return out, true
}
