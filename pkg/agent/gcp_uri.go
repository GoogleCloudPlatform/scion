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

var validGCPComponent = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// GCPSkillRef is the parsed representation of a GCP skill URI.
type GCPSkillRef struct {
	Alias   string // Registry alias name (e.g., "team-skills")
	SkillID string // GCP Skill resource ID
	Version string // Optional version constraint
	Raw     string // Original URI
}

// ParseGCPSkillURI parses a gcp-skill:// URI into its components.
//
// Grammar:
//
//	gcp-skill://alias/SKILL_ID[@version]
func ParseGCPSkillURI(uri string) (*GCPSkillRef, error) {
	const prefix = "gcp-skill://"
	if !strings.HasPrefix(uri, prefix) {
		return nil, fmt.Errorf("not a gcp-skill URI: %q", uri)
	}

	rest := strings.TrimPrefix(uri, prefix)

	// Split off @version
	var version string
	if idx := strings.LastIndex(rest, "@"); idx >= 0 {
		version = rest[idx+1:]
		rest = rest[:idx]
		if version == "" {
			return nil, fmt.Errorf("invalid gcp-skill URI %q: empty version after @", uri)
		}
	}

	// Split alias/skill-id
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("invalid gcp-skill URI %q: expected gcp-skill://alias/SKILL_ID", uri)
	}

	if strings.Contains(parts[1], "/") {
		return nil, fmt.Errorf("invalid gcp-skill URI %q: SKILL_ID must not contain slashes", uri)
	}

	if !validGCPComponent.MatchString(parts[0]) {
		return nil, fmt.Errorf("invalid gcp-skill URI %q: invalid alias %q", uri, parts[0])
	}
	if !validGCPComponent.MatchString(parts[1]) {
		return nil, fmt.Errorf("invalid gcp-skill URI %q: invalid skill ID %q", uri, parts[1])
	}

	return &GCPSkillRef{
		Alias:   parts[0],
		SkillID: parts[1],
		Version: version,
		Raw:     uri,
	}, nil
}
