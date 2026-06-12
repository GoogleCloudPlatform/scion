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
	"regexp"
	"strings"
)

var validGitHubComponent = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

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
	if !validGitHubComponent.MatchString(parts[0]) {
		return nil, fmt.Errorf("invalid gh:// URI %q: invalid owner %q", uri, parts[0])
	}
	if !validGitHubComponent.MatchString(parts[1]) {
		return nil, fmt.Errorf("invalid gh:// URI %q: invalid repo %q", uri, parts[1])
	}
	if strings.Contains(parts[2], "..") {
		return nil, fmt.Errorf("invalid gh:// URI %q: skill name must not contain '..'", uri)
	}
	if strings.ContainsAny(parts[2], "?#&=") {
		return nil, fmt.Errorf("invalid gh:// URI %q: skill name contains invalid characters", uri)
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

// parseGitHubFullURL parses a full GitHub URL into a GitHubSkillRef.
// Supports:
//
//	https://github.com/owner/repo/tree/ref/path/to/skill-name
//	https://github.com/owner/repo/tree/ref/skills/skill-name
func parseGitHubFullURL(uri string) (*GitHubSkillRef, error) {
	rest := uri
	for _, prefix := range []string{"https://github.com/", "http://github.com/"} {
		if strings.HasPrefix(rest, prefix) {
			rest = strings.TrimPrefix(rest, prefix)
			break
		}
	}

	// Expected: owner/repo/tree/ref/path/to/skill-name
	parts := strings.SplitN(rest, "/", 5)
	if len(parts) < 5 || parts[2] != "tree" {
		return nil, fmt.Errorf("invalid GitHub URL %q: expected https://github.com/owner/repo/tree/ref/path/to/skill", uri)
	}

	owner := parts[0]
	repo := parts[1]
	if !validGitHubComponent.MatchString(owner) {
		return nil, fmt.Errorf("invalid GitHub URL %q: invalid owner %q", uri, owner)
	}
	if !validGitHubComponent.MatchString(repo) {
		return nil, fmt.Errorf("invalid GitHub URL %q: invalid repo %q", uri, repo)
	}
	refAndPath := parts[3] + "/" + parts[4]

	// Split ref from path: assume first segment is the ref.
	// For ambiguous cases (multi-segment refs), use gh:// shorthand with @ref.
	refParts := strings.SplitN(refAndPath, "/", 2)
	if len(refParts) < 2 {
		return nil, fmt.Errorf("invalid GitHub URL %q: missing skill path after ref", uri)
	}
	ref := refParts[0]
	skillFullPath := refParts[1]

	pathParts := strings.Split(skillFullPath, "/")
	skillName := pathParts[len(pathParts)-1]
	if skillName == "" {
		return nil, fmt.Errorf("invalid GitHub URL %q: empty skill name", uri)
	}
	if strings.Contains(skillFullPath, "..") {
		return nil, fmt.Errorf("invalid GitHub URL %q: path must not contain '..'", uri)
	}

	return &GitHubSkillRef{
		Owner:     owner,
		Repo:      repo,
		SkillName: skillName,
		Ref:       ref,
		SkillPath: skillFullPath,
		Raw:       uri,
	}, nil
}
