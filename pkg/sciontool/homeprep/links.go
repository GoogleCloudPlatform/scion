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

package homeprep

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// Link is one entry of SCION_HOME_LINKS: a symbolic link at Target (relative
// to the home) pointing to the staged file Source. Mode is the declared mode
// of the staged file; it is not applied to the link.
type Link struct {
	Target string `json:"target"`
	Source string `json:"source"`
	Mode   string `json:"mode"`
}

// Link states in the result file.
const (
	LinkLinked  = "linked"
	LinkSkipped = "skipped"
)

// LinkResult is one entry of the link result file.
type LinkResult struct {
	Target string `json:"target"`
	State  string `json:"state"`
}

// LinksResult is the content of LinksResultFileName.
type LinksResult struct {
	Links []LinkResult `json:"links"`
}

// linksRecord is the content of LinksRecordPath: the targets a start
// placed as links, and that start's ID.
type linksRecord struct {
	StartID string   `json:"start_id"`
	Links   []string `json:"links"`
}

// ParseLinks parses and validates SCION_HOME_LINKS. Targets must be
// relative paths inside the home, outside the reserved names and the link
// record; sources must be absolute. Duplicate targets are rejected.
func ParseLinks(raw string) ([]Link, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var links []Link
	if err := json.Unmarshal([]byte(raw), &links); err != nil {
		return nil, fmt.Errorf("invalid home links: %w", err)
	}
	seen := map[string]bool{}
	for i, l := range links {
		t, err := cleanRelPath(l.Target)
		if err != nil {
			return nil, fmt.Errorf("invalid home link target %q: %w", l.Target, err)
		}
		first := strings.SplitN(t, "/", 2)[0]
		if strings.HasPrefix(first, ReservedPrefix) || t == LinksRecordPath {
			return nil, fmt.Errorf("home link target %q uses a reserved name", l.Target)
		}
		if !path.IsAbs(l.Source) || path.Clean(l.Source) != l.Source {
			return nil, fmt.Errorf("home link source %q must be a clean absolute path", l.Source)
		}
		if seen[t] {
			return nil, fmt.Errorf("duplicate home link target %q", t)
		}
		seen[t] = true
		links[i].Target = t
	}
	// A target inside another target cannot be placed together with it:
	// one would have to be a directory and a link at once.
	for _, a := range links {
		for _, b := range links {
			if strings.HasPrefix(b.Target, a.Target+"/") {
				return nil, fmt.Errorf("home link target %q is inside home link target %q", b.Target, a.Target)
			}
		}
	}
	return links, nil
}

// cleanRelPath is the platform-independent part of the root path check.
func cleanRelPath(rel string) (string, error) {
	if rel == "" || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("must be a relative path")
	}
	c := path.Clean(rel)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("outside the home")
	}
	return c, nil
}
