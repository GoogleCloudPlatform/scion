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

package agent

import (
	"encoding/json"
	"net/http"
)

// fakeTreeFile is one entry a fake GitHub server lists for a skill
// directory. writeTestTree renders it as a git trees API entry whose path
// is Name, so it describes a file directly in the skill directory; tests
// of nested paths write the tree JSON themselves. Path is the
// repository path and is only informational.
type fakeTreeFile struct {
	Name string
	Path string
	// Type is "file" (a blob), "dir" (a tree), "symlink" (a blob with
	// mode 120000) or "submodule" (a commit).
	Type string
	Size int
	// Mode is the git mode of a "file"; empty means "100644".
	Mode string
}

type fakeTreeEntry struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int    `json:"size,omitempty"`
}

// testTreeJSON renders files as a git trees API response body.
func testTreeJSON(files []fakeTreeFile) ([]byte, error) {
	entries := make([]fakeTreeEntry, 0, len(files))
	for _, f := range files {
		e := fakeTreeEntry{Path: f.Name, SHA: "0000000000000000000000000000000000000000", Size: f.Size}
		switch f.Type {
		case "dir":
			e.Type, e.Mode = "tree", "040000"
		case "symlink":
			e.Type, e.Mode = "blob", "120000"
		case "submodule":
			e.Type, e.Mode = "commit", "160000"
		default:
			e.Type, e.Mode = "blob", f.Mode
			if e.Mode == "" {
				e.Mode = "100644"
			}
		}
		entries = append(entries, e)
	}
	return json.Marshal(map[string]any{"sha": "tree", "tree": entries, "truncated": false})
}

// writeTestTree writes files to w as a git trees API response.
func writeTestTree(w http.ResponseWriter, files []fakeTreeFile) error {
	body, err := testTreeJSON(files)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(body)
	return err
}
