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

package api

import (
	"fmt"
	"regexp"
	"strings"
)

// validGHComponent matches valid GitHub owner and repo name characters.
// NOTE: this must be kept in sync with validGitHubComponent in
// pkg/agent/github_uri.go.
var validGHComponent = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// validGHSecretName matches token secret names: [A-Z][A-Z0-9_]*.
// NOTE: this must be kept in sync with validTokenSecretName in
// pkg/agent/github_uri.go.
var validGHSecretName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// NormalizeSkillURI accepts a user-supplied skill URI in any supported input
// form and returns the canonical stored form.
//
// Supported input → canonical output:
//
//	gh://owner/repo/skill[@ref][?token=SECRET_NAME]
//	  → gh://owner/repo/skill[@ref][?token=SECRET_NAME]   (validated, returned as-is)
//
//	https://github.com/owner/repo/tree/ref/skills/skill-name[?token=SECRET_NAME]
//	  → gh://owner/repo/skill-name@ref[?token=SECRET_NAME]   (shorthand)
//
//	https://github.com/owner/repo/tree/ref/other/path[?token=SECRET_NAME]
//	  → https://github.com/owner/repo/tree/ref/other/path[?token=SECRET_NAME]   (kept)
//
//	https://github.com/owner/repo/blob/ref/skills/skill-name/SKILL.md[?token=SECRET_NAME]
//	  → gh://owner/repo/skill-name@ref[?token=SECRET_NAME]   (blob: last segment stripped)
//
//	skill://[registry/][scope/]name[@version]  or  bare-name
//	  → validated via ParseSkillURI, returned as-is
//
// Returns a non-nil error with an actionable message for unsupported or
// ambiguous inputs (bare repo URL, unknown scheme, invalid component, etc.).
func NormalizeSkillURI(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", fmt.Errorf("skill URI must not be empty")
	}

	lower := strings.ToLower(input)

	switch {
	case strings.HasPrefix(lower, "gh://"):
		return normalizeGHShorthand(input)
	case strings.HasPrefix(lower, "https://github.com/"),
		strings.HasPrefix(lower, "http://github.com/"):
		return normalizeGitHubURL(input)
	case strings.HasPrefix(lower, "scion://"):
		return "", fmt.Errorf("scion:// is not a supported scheme; use skill:// for hub-bank skills")
	case strings.HasPrefix(lower, "skill://"):
		if _, err := ParseSkillURI(input); err != nil {
			return "", err
		}
		return input, nil
	case strings.Contains(input, "://"):
		// Unknown scheme
		scheme := input[:strings.Index(input, "://")]
		return "", fmt.Errorf("unsupported scheme %q; supported inputs: gh://owner/repo/skill, skill://name, or a GitHub URL (https://github.com/...)", scheme)
	default:
		// Bare skill name
		if _, err := ParseSkillURI(input); err != nil {
			return "", err
		}
		return input, nil
	}
}

// normalizeGHShorthand validates a gh:// URI and returns it unchanged if valid.
// The grammar is: gh://owner/repo/skill-name[@ref][?token=SECRET_NAME]
// This mirrors parseGHShorthand in pkg/agent/github_uri.go.
func normalizeGHShorthand(uri string) (string, error) {
	rest := uri[len("gh://"):]

	// Extract ?token=SECRET_NAME query param, which must be the last component.
	var tokenSuffix string
	if qIdx := strings.Index(rest, "?"); qIdx >= 0 {
		query := rest[qIdx+1:]
		rest = rest[:qIdx]
		if !strings.HasPrefix(query, "token=") {
			return "", fmt.Errorf("invalid gh:// URI %q: unsupported query parameter; only ?token=SECRET_NAME is supported", uri)
		}
		tokenName := query[len("token="):]
		if tokenName == "" {
			return "", fmt.Errorf("invalid gh:// URI %q: empty ?token= value", uri)
		}
		if !validGHSecretName.MatchString(tokenName) {
			return "", fmt.Errorf("invalid gh:// URI %q: ?token= value %q must match [A-Z][A-Z0-9_]* (uppercase env-var style)", uri, tokenName)
		}
		tokenSuffix = "?" + query
	}

	// Extract @ref (split at last @, which must be after the skill-name segment).
	var refSuffix string
	if idx := strings.LastIndex(rest, "@"); idx >= 0 {
		ref := rest[idx+1:]
		rest = rest[:idx]
		if ref == "" || ref == "." || ref == ".." {
			return "", fmt.Errorf("invalid gh:// URI %q: invalid ref %q", uri, ref)
		}
		refSuffix = "@" + ref
	}

	// Expect exactly owner/repo/skill-name (3 slash-separated segments).
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", fmt.Errorf("invalid gh:// URI %q: expected gh://owner/repo/skill-name[@ref][?token=SECRET_NAME]", uri)
	}
	for _, p := range parts {
		if p == "" {
			return "", fmt.Errorf("invalid gh:// URI %q: empty path component", uri)
		}
	}
	if !validGHComponent.MatchString(parts[0]) || parts[0] == "." || parts[0] == ".." {
		return "", fmt.Errorf("invalid gh:// URI %q: invalid owner %q (must match [a-zA-Z0-9._-]+)", uri, parts[0])
	}
	if !validGHComponent.MatchString(parts[1]) || parts[1] == "." || parts[1] == ".." {
		return "", fmt.Errorf("invalid gh:// URI %q: invalid repo %q (must match [a-zA-Z0-9._-]+)", uri, parts[1])
	}
	if parts[2] == "." || strings.Contains(parts[2], "..") || strings.ContainsAny(parts[2], "?#&=") {
		return "", fmt.Errorf("invalid gh:// URI %q: invalid skill name %q", uri, parts[2])
	}

	return "gh://" + rest + refSuffix + tokenSuffix, nil
}

// normalizeGitHubURL transforms a full GitHub URL (tree or blob form) into
// the canonical stored form.
//
// tree URLs with a skills/skill-name path → gh://owner/repo/skill-name@ref
// tree URLs with any other path            → full URL (kept, with https://)
// blob URLs                               → last segment stripped, then same rules as tree
func normalizeGitHubURL(uri string) (string, error) {
	// Determine the path part after the scheme+host, preserving original case.
	var rest string
	for _, prefix := range []string{"https://github.com/", "http://github.com/"} {
		if strings.HasPrefix(strings.ToLower(uri), prefix) {
			rest = uri[len(prefix):]
			break
		}
	}

	// Extract ?token=SECRET_NAME before path parsing.
	var tokenSuffix string
	if qIdx := strings.Index(rest, "?"); qIdx >= 0 {
		query := rest[qIdx+1:]
		rest = rest[:qIdx]
		if !strings.HasPrefix(query, "token=") {
			return "", fmt.Errorf("invalid GitHub URL %q: unsupported query parameter; only ?token=SECRET_NAME is supported", uri)
		}
		tokenName := query[len("token="):]
		if tokenName == "" {
			return "", fmt.Errorf("invalid GitHub URL %q: empty ?token= value", uri)
		}
		if !validGHSecretName.MatchString(tokenName) {
			return "", fmt.Errorf("invalid GitHub URL %q: ?token= value %q must match [A-Z][A-Z0-9_]* (uppercase env-var style)", uri, tokenName)
		}
		tokenSuffix = "?" + query
	}

	// Split: owner / repo / keyword / ref / path...
	// Use SplitN 5 so the path portion is kept whole.
	parts := strings.SplitN(rest, "/", 5)
	if len(parts) < 4 {
		return "", fmt.Errorf("invalid GitHub URL %q: expected https://github.com/owner/repo/tree/ref/path/to/skill", uri)
	}

	owner := parts[0]
	repo := parts[1]
	keyword := strings.ToLower(parts[2])
	ref := parts[3]

	if !validGHComponent.MatchString(owner) || owner == "." || owner == ".." {
		return "", fmt.Errorf("invalid GitHub URL %q: invalid owner %q", uri, owner)
	}
	if !validGHComponent.MatchString(repo) || repo == "." || repo == ".." {
		return "", fmt.Errorf("invalid GitHub URL %q: invalid repo %q", uri, repo)
	}
	if ref == "" || ref == "." || ref == ".." {
		return "", fmt.Errorf("invalid GitHub URL %q: empty or invalid ref %q", uri, ref)
	}

	var skillFullPath string
	switch keyword {
	case "tree":
		if len(parts) < 5 || parts[4] == "" {
			return "", fmt.Errorf("invalid GitHub URL %q: missing skill path after ref; example: https://github.com/owner/repo/tree/main/skills/my-skill", uri)
		}
		skillFullPath = parts[4]
	case "blob":
		if len(parts) < 5 || parts[4] == "" {
			return "", fmt.Errorf("invalid GitHub URL %q: missing file path after ref", uri)
		}
		filePath := parts[4]
		// Blob URLs always end in a filename; strip the last segment to get
		// the skill directory.
		if idx := strings.LastIndex(filePath, "/"); idx >= 0 {
			filePath = filePath[:idx]
		} else {
			// No slash means the file is at the repo root — no skill directory.
			return "", fmt.Errorf("invalid GitHub URL %q: cannot determine skill directory from blob URL (no parent directory)", uri)
		}
		if filePath == "" {
			return "", fmt.Errorf("invalid GitHub URL %q: cannot determine skill directory from blob URL", uri)
		}
		skillFullPath = filePath
	default:
		return "", fmt.Errorf("invalid GitHub URL %q: expected /tree/ or /blob/ after owner/repo, got /%s/", uri, parts[2])
	}

	if strings.Contains(skillFullPath, "..") {
		return "", fmt.Errorf("invalid GitHub URL %q: path must not contain '..'", uri)
	}

	// Try to produce the compact gh:// shorthand.
	// The shorthand maps gh://owner/repo/skill-name → skills/skill-name in the
	// repo (implicit "skills/" prefix). Only use it for paths of exactly that form.
	pathParts := strings.Split(skillFullPath, "/")
	if len(pathParts) == 2 && strings.ToLower(pathParts[0]) == "skills" && pathParts[1] != "" {
		skillName := pathParts[1]
		if strings.Contains(skillName, "..") || strings.ContainsAny(skillName, "?#&=") {
			return "", fmt.Errorf("invalid GitHub URL %q: invalid skill name %q", uri, skillName)
		}
		return "gh://" + owner + "/" + repo + "/" + skillName + "@" + ref + tokenSuffix, nil
	}

	// Non-standard path: keep as full canonical tree URL (resolver handles it).
	return "https://github.com/" + owner + "/" + repo + "/tree/" + ref + "/" + skillFullPath + tokenSuffix, nil
}
