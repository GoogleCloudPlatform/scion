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

package grant

import (
	"errors"
	"fmt"
	"slices"
)

// Scope boundary kinds.
const (
	ScopeBoundaryProject = "project"
	ScopeBoundaryHub     = "hub"
)

// ErrScope refuses a renewal whose scope claim differs from the admitted
// open grant's, including one present on one side only.
var ErrScope = errors.New("grant: scope does not match")

// ErrTooLarge refuses to mint a token larger than verifiers accept.
var ErrTooLarge = errors.New("grant: token exceeds the size limit")

// Scope is the credential context of a user stream opened with a scoped
// token (design v2.21.31 §3.5, Scope claim scope): the token's scopes, its
// boundary and its permission ceiling, as the hub's scoped-token check
// consumes them. Open grants to stream_renewal.v2 targets carry it for such
// streams, every renewal copies it unchanged, and a target compares it
// whole without interpreting it. Streams opened without a scoped token
// carry none.
type Scope struct {
	// Scopes is the token's scope list, deduplicated and sorted.
	Scopes   []string      `json:"scopes"`
	Boundary ScopeBoundary `json:"boundary"`
	Ceiling  ScopeCeiling  `json:"ceiling"`
}

// ScopeBoundary is the token's boundary: ScopeBoundaryProject with its
// ProjectID, or ScopeBoundaryHub with no ProjectID.
type ScopeBoundary struct {
	Kind      string `json:"kind"`
	ProjectID string `json:"project_id,omitempty"`
}

// ScopeCeiling is the token's permission ceiling as stored: its version
// and its permission ids, deduplicated and sorted.
type ScopeCeiling struct {
	Version       int      `json:"version"`
	PermissionIDs []string `json:"permission_ids"`
}

// validate checks the scope's shape: a non-empty sorted, duplicate-free
// scope list; a project boundary with a project id or a hub boundary
// without one; a non-negative ceiling version; and a present, sorted,
// duplicate-free permission id list (which may be empty).
func (s *Scope) validate() error {
	switch {
	case len(s.Scopes) == 0 || !sortedUnique(s.Scopes):
		return fmt.Errorf("%w: scope.scopes must be a non-empty sorted list without duplicates", ErrInvalidClaims)
	case s.Boundary.Kind == ScopeBoundaryProject && s.Boundary.ProjectID == "":
		return fmt.Errorf("%w: project scope boundary without project_id", ErrInvalidClaims)
	case s.Boundary.Kind == ScopeBoundaryHub && s.Boundary.ProjectID != "":
		return fmt.Errorf("%w: hub scope boundary with project_id", ErrInvalidClaims)
	case s.Boundary.Kind != ScopeBoundaryProject && s.Boundary.Kind != ScopeBoundaryHub:
		return fmt.Errorf("%w: unknown scope boundary kind %q", ErrInvalidClaims, s.Boundary.Kind)
	case s.Ceiling.Version < 0:
		return fmt.Errorf("%w: negative scope ceiling version", ErrInvalidClaims)
	case s.Ceiling.PermissionIDs == nil || !sortedUnique(s.Ceiling.PermissionIDs):
		return fmt.Errorf("%w: scope.ceiling.permission_ids must be a sorted list without duplicates", ErrInvalidClaims)
	}
	return nil
}

// Validate reports whether s is a well-formed scope claim (see validate).
func (s *Scope) Validate() error { return s.validate() }

// ScopeEqual reports whether a and b are the same scope claim: both absent,
// or both present with equal fields.
func ScopeEqual(a, b *Scope) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return slices.Equal(a.Scopes, b.Scopes) && a.Boundary == b.Boundary &&
		a.Ceiling.Version == b.Ceiling.Version && slices.Equal(a.Ceiling.PermissionIDs, b.Ceiling.PermissionIDs)
}

// NewScope returns the scope claim for a token's scopes, boundary and
// ceiling: the two lists deduplicated and sorted, and the ceiling's
// version unchanged. It does not validate the result; Mint does.
func NewScope(scopes []string, boundary ScopeBoundary, ceilingVersion int, permissionIDs []string) *Scope {
	return &Scope{
		Scopes:   sortUnique(scopes),
		Boundary: boundary,
		Ceiling:  ScopeCeiling{Version: ceilingVersion, PermissionIDs: sortUnique(permissionIDs)},
	}
}

func sortedUnique(v []string) bool {
	for i := 1; i < len(v); i++ {
		if v[i-1] >= v[i] {
			return false
		}
	}
	return true
}

// sortUnique returns a sorted copy of v without duplicates, never nil.
func sortUnique(v []string) []string {
	out := slices.Clone(v)
	slices.Sort(out)
	out = slices.Compact(out)
	if out == nil {
		out = []string{}
	}
	return out
}
