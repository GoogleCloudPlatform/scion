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

// SkillURI is the parsed representation of a skill reference URI.
type SkillURI struct {
	Registry string // "scion", "registry.example.com", etc. Default: "scion"
	Scope    string // "core", "global", "project", "user", or "" (search)
	ScopeID  string // project ID or user ID; empty for core/global or search
	Name     string // kebab-case skill name
	Version  string // "1.2.3", "^1.0", "latest", "sha256:...", etc. Default: "latest"
	Raw      string // original input for error messages
}

const (
	skillURIScheme  = "skill://"
	defaultRegistry = "scion"
	defaultVersion  = "latest"
	maxSkillNameLen = 64
)

var validScopes = map[string]bool{
	"core":    true,
	"global":  true,
	"project": true,
	"user":    true,
}

var registryAliases = map[string]string{
	"project": "project",
	"user":    "user",
}

var skillNameRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// ValidateSkillName checks that a string is a valid skill name:
// kebab-case, 1-64 chars, [a-z0-9]([a-z0-9-]*[a-z0-9])? pattern.
func ValidateSkillName(name string) error {
	if name == "" {
		return fmt.Errorf("skill name must not be empty")
	}
	if len(name) > maxSkillNameLen {
		return fmt.Errorf("skill name %q exceeds maximum length of %d characters", name, maxSkillNameLen)
	}
	if !skillNameRegexp.MatchString(name) {
		return fmt.Errorf("skill name %q must be kebab-case (lowercase alphanumeric with hyphens, no leading/trailing hyphens)", name)
	}
	return nil
}

// ParseSkillURI parses a skill URI string into its components.
// Accepts all forms from the normative grammar:
//   - Full:      skill://scion/core/scion@^1.0
//   - No reg:    skill:///core/scion@^1.0
//   - No ver:    skill://scion/core/scion
//   - Alias:     skill://project/my-skill@latest
//   - Bare:      scion
//
// Returns an error for invalid URIs (empty name, invalid scope, bad chars).
func ParseSkillURI(raw string) (*SkillURI, error) {
	if raw == "" {
		return nil, fmt.Errorf("skill URI must not be empty")
	}

	uri := &SkillURI{Raw: raw}

	if strings.HasPrefix(raw, skillURIScheme) {
		return parseFullURI(raw, uri)
	}

	// Bare name form
	if strings.Contains(raw, "://") {
		return nil, fmt.Errorf("invalid skill URI %q: unsupported scheme (must use %q or bare name)", raw, "skill://")
	}
	if strings.Contains(raw, "/") || strings.Contains(raw, "..") {
		return nil, fmt.Errorf("invalid skill URI %q: bare names must not contain path separators or traversals", raw)
	}
	if err := ValidateSkillName(raw); err != nil {
		return nil, fmt.Errorf("invalid skill URI %q: %w", raw, err)
	}
	uri.Registry = defaultRegistry
	uri.Name = raw
	uri.Version = defaultVersion
	return uri, nil
}

func parseFullURI(raw string, uri *SkillURI) (*SkillURI, error) {
	rest := raw[len(skillURIScheme):]

	// Split off version at @ after the last path separator so that
	// authority credentials (skill://user:pass@host/...) are not confused
	// with version specifiers.
	var version string
	lastSlash := strings.LastIndex(rest, "/")
	tail := rest
	if lastSlash >= 0 {
		tail = rest[lastSlash:]
	}
	if idx := strings.LastIndex(tail, "@"); idx >= 0 {
		absIdx := idx
		if lastSlash >= 0 {
			absIdx += lastSlash
		}
		version = rest[absIdx+1:]
		rest = rest[:absIdx]
		if version == "" {
			return nil, fmt.Errorf("invalid skill URI %q: empty version after @", raw)
		}
	}

	// Strip leading 'v' prefix from version
	if version != "" {
		version = stripVersionPrefix(version)
	}

	// Split path segments
	segments := strings.Split(rest, "/")

	// First segment is the registry (may be empty for skill:///)
	registry := segments[0]
	pathSegments := segments[1:]

	// Handle registry aliases
	if alias, ok := registryAliases[registry]; ok {
		uri.Scope = alias
		uri.Registry = defaultRegistry
		registry = defaultRegistry

		// For alias forms like skill://project/my-skill, pathSegments are the rest
		return parseAliasPath(raw, uri, pathSegments, version)
	}

	if registry == "" {
		registry = defaultRegistry
	}
	uri.Registry = registry

	return parseScopedPath(raw, uri, pathSegments, version)
}

func parseAliasPath(raw string, uri *SkillURI, segments []string, version string) (*SkillURI, error) {
	switch len(segments) {
	case 0:
		return nil, fmt.Errorf("invalid skill URI %q: missing skill name", raw)
	case 1:
		// skill://project/my-skill — no scope ID
		uri.Name = segments[0]
	case 2:
		// skill://project/my-proj-id/my-skill — with scope ID
		uri.ScopeID = segments[0]
		uri.Name = segments[1]
	default:
		return nil, fmt.Errorf("invalid skill URI %q: too many path segments", raw)
	}

	if err := ValidateSkillName(uri.Name); err != nil {
		return nil, fmt.Errorf("invalid skill URI %q: %w", raw, err)
	}

	if version == "" {
		uri.Version = defaultVersion
	} else {
		uri.Version = version
	}
	return uri, nil
}

func parseScopedPath(raw string, uri *SkillURI, segments []string, version string) (*SkillURI, error) {
	switch len(segments) {
	case 0:
		return nil, fmt.Errorf("invalid skill URI %q: missing skill name", raw)
	case 1:
		// skill://scion/my-skill — scope is empty (search order)
		uri.Name = segments[0]
	case 2:
		// skill://scion/core/my-skill — with scope keyword
		scope := segments[0]
		if !validScopes[scope] {
			return nil, fmt.Errorf("invalid skill URI %q: unrecognized scope %q (must be core, global, project, or user)", raw, scope)
		}
		uri.Scope = scope
		uri.Name = segments[1]
	case 3:
		// skill://scion/project/my-proj-id/my-skill — with scope keyword and scope ID
		scope := segments[0]
		if !validScopes[scope] {
			return nil, fmt.Errorf("invalid skill URI %q: unrecognized scope %q (must be core, global, project, or user)", raw, scope)
		}
		uri.Scope = scope
		uri.ScopeID = segments[1]
		uri.Name = segments[2]
	default:
		return nil, fmt.Errorf("invalid skill URI %q: too many path segments", raw)
	}

	if err := ValidateSkillName(uri.Name); err != nil {
		return nil, fmt.Errorf("invalid skill URI %q: %w", raw, err)
	}

	if version == "" {
		uri.Version = defaultVersion
	} else {
		uri.Version = version
	}
	return uri, nil
}

// SkillURIScheme returns the raw scheme prefix of a skill URI.
// Note: This is NOT used for routing dispatch. The RoutingSkillResolver
// uses detectScheme() which maps full GitHub URLs to the 'gh' scheme.
// This function is a lightweight utility for non-routing scheme checks.
func SkillURIScheme(uri string) string {
	if idx := strings.Index(uri, "://"); idx > 0 {
		return uri[:idx]
	}
	return "skill"
}

func stripVersionPrefix(v string) string {
	if strings.HasPrefix(v, "v") && len(v) > 1 {
		next := v[1]
		if next >= '0' && next <= '9' {
			return v[1:]
		}
	}
	return v
}
