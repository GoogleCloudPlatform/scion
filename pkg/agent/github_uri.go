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
	"fmt"
	"strings"
)

// GitHubSkillRef is the parsed representation of a GitHub skill URI.
type GitHubSkillRef struct {
	Owner     string // GitHub user or organization
	Repo      string // Repository name
	SkillName string // Directory name under skills/
	Ref       string // Branch, tag, or commit SHA; empty = default branch
	SkillPath string // Full path within repo (default: "skills/{SkillName}")
	Raw       string // Original URI for error messages
}

// ParseGitHubSkillURI parses a gh:// shorthand or full GitHub URL
// into a GitHubSkillRef.
func ParseGitHubSkillURI(uri string) (*GitHubSkillRef, error) {
	if strings.HasPrefix(uri, "gh://") {
		return parseGHShorthand(uri)
	}
	if strings.HasPrefix(uri, "https://github.com/") || strings.HasPrefix(uri, "http://github.com/") {
		return parseGitHubFullURL(uri)
	}
	return nil, fmt.Errorf("not a GitHub skill URI: %q", uri)
}

func parseGHShorthand(uri string) (*GitHubSkillRef, error) {
	rest := strings.TrimPrefix(uri, "gh://")

	// Split off @ref
	var ref string
	if idx := strings.LastIndex(rest, "@"); idx >= 0 {
		ref = rest[idx+1:]
		rest = rest[:idx]
		if ref == "" {
			return nil, fmt.Errorf("invalid gh:// URI %q: empty ref after @", uri)
		}
	}

	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid gh:// URI %q: expected gh://owner/repo/skill-name[@ref]", uri)
	}
	for _, p := range parts {
		if p == "" {
			return nil, fmt.Errorf("invalid gh:// URI %q: empty path component", uri)
		}
	}

	return &GitHubSkillRef{
		Owner:     parts[0],
		Repo:      parts[1],
		SkillName: parts[2],
		Ref:       ref,
		SkillPath: "skills/" + parts[2],
		Raw:       uri,
	}, nil
}

func parseGitHubFullURL(uri string) (*GitHubSkillRef, error) {
	return nil, fmt.Errorf("full GitHub URL parsing not yet implemented: %q", uri)
}
