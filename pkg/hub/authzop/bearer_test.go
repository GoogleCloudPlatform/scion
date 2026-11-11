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

package authzop

import (
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
)

// bearerDispositionProblems returns every reason the spec's bearer
// disposition is not acceptable: an unknown or incomplete kind, an admit
// whose BasePermission has no selector or whose boundaries are not all
// allowed for that permission, or a non-empty disposition on an operation
// listed in PendingBearerOperations.
func bearerDispositionProblems(spec OperationSpec, pending map[OperationID]bool) []string {
	var problems []string
	if pending[spec.ID] {
		if !spec.Bearer.IsZero() {
			problems = append(problems, "listed in PendingBearerOperations but declares a disposition")
		}
		return problems
	}
	for _, err := range spec.Bearer.Validate() {
		problems = append(problems, err.Error())
	}
	if spec.Bearer.Kind != BearerAdmit {
		return problems
	}
	perm, ok := registryPermission(spec.BasePermission)
	if !ok {
		return append(problems, "admit requires a registered BasePermission, got "+spec.BasePermission)
	}
	if perm.UATScope == "" {
		problems = append(problems, "admit requires a BasePermission with a selector; "+spec.BasePermission+" has none")
	}
	allowed, listed := permissions.SelectorAllowedBoundaries(spec.BasePermission)
	if !listed {
		problems = append(problems, "admit requires allowed boundaries listed for "+spec.BasePermission)
	}
	for _, b := range spec.Bearer.Boundaries {
		found := false
		for _, a := range allowed {
			if string(a) == string(b) {
				found = true
			}
		}
		if !found {
			problems = append(problems, "boundary "+string(b)+" is not allowed for "+spec.BasePermission)
		}
	}
	return problems
}

func registryPermission(id string) (permissions.Permission, bool) {
	for _, p := range permissions.Registry {
		if p.ID == id {
			return p, true
		}
	}
	return permissions.Permission{}, false
}

func pendingOperationSet() map[OperationID]bool {
	m := make(map[OperationID]bool, len(PendingBearerOperations))
	for _, id := range PendingBearerOperations {
		m[id] = true
	}
	return m
}

// TestBearerDisposition_EveryOperationDeclaresOne requires every catalog
// operation to declare a valid bearer disposition, or to be listed in
// PendingBearerOperations with none. An admit disposition needs a
// BasePermission with a selector, and its boundaries must be allowed for
// that permission. A session_only disposition needs a known reason and an
// out_of_scope disposition needs a known owner. The pending list names only
// catalogued operations, once each.
func TestBearerDisposition_EveryOperationDeclaresOne(t *testing.T) {
	pending := pendingOperationSet()
	ids := CatalogOperationIDs()

	seen := make(map[OperationID]bool)
	for _, id := range PendingBearerOperations {
		if seen[id] {
			t.Errorf("PendingBearerOperations lists %q twice", id)
		}
		seen[id] = true
		if !ids[id] {
			t.Errorf("PendingBearerOperations lists %q, which is not a catalog operation", id)
		}
	}

	declared := 0
	for _, spec := range Catalog {
		for _, p := range bearerDispositionProblems(spec, pending) {
			t.Errorf("operation %s: %s", spec.ID, p)
		}
		if !pending[spec.ID] {
			declared++
		}
	}
	if declared == 0 {
		t.Fatal("no catalog operation declares a bearer disposition")
	}
	t.Logf("bearer dispositions: %d declared, %d pending", declared, len(PendingBearerOperations))
}

// TestBearerDisposition_RuleRejectsInvalidDispositions shows that the rule
// behind TestBearerDisposition_EveryOperationDeclaresOne rejects each kind
// of invalid disposition.
func TestBearerDisposition_RuleRejectsInvalidDispositions(t *testing.T) {
	base := func() OperationSpec {
		s := validSpec()
		s.BasePermission = "agent.read"
		s.Credentials = []CredentialKind{CredentialSessionJWT, CredentialScopedUAT}
		s.Bearer = AdmitOn(BearerTargetAgentRecord, BearerBoundaryProject, BearerBoundaryHub)
		return s
	}
	if p := bearerDispositionProblems(base(), nil); len(p) != 0 {
		t.Fatalf("baseline admit should be accepted, got %v", p)
	}

	cases := []struct {
		name   string
		mutate func(*OperationSpec)
		want   string
	}{
		{"missing kind", func(s *OperationSpec) { s.Bearer = BearerDisposition{} }, "kind is required"},
		{"unknown kind", func(s *OperationSpec) { s.Bearer = BearerDisposition{Kind: "maybe"} }, "unknown kind"},
		{"admit without selector", func(s *OperationSpec) { s.BasePermission = "project.delete" }, "has none"},
		{"admit on unregistered permission", func(s *OperationSpec) { s.BasePermission = "nope.read" }, "registered BasePermission"},
		{"admit boundary not allowed", func(s *OperationSpec) {
			s.BasePermission = "group.read"
			s.Bearer = AdmitOn(BearerTargetHubInstance, BearerBoundaryProject)
		}, "boundary project is not allowed"},
		{"admit without boundaries", func(s *OperationSpec) { s.Bearer.Boundaries = nil }, "at least one boundary"},
		{"admit without target", func(s *OperationSpec) { s.Bearer.Target = "" }, "known target"},
		{"session_only without reason", func(s *OperationSpec) { s.Bearer = BearerDisposition{Kind: BearerSessionOnly} }, "known reason"},
		{"session_only unknown reason", func(s *OperationSpec) { s.Bearer = SessionOnly("BECAUSE") }, "known reason"},
		{"out_of_scope without owner", func(s *OperationSpec) { s.Bearer = BearerDisposition{Kind: BearerOutOfScope} }, "known owner"},
		{"admit_self without filter", func(s *OperationSpec) { s.Bearer = BearerDisposition{Kind: BearerAdmitSelf} }, "known self filter"},
		{"field of another kind", func(s *OperationSpec) {
			s.Bearer = BearerDisposition{Kind: BearerNonUser, Reason: ReasonGovernancePending}
		}, "reason applies only to session_only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base()
			tc.mutate(&s)
			problems := bearerDispositionProblems(s, nil)
			if !strings.Contains(strings.Join(problems, "; "), tc.want) {
				t.Errorf("want a problem containing %q, got %v", tc.want, problems)
			}
		})
	}

	t.Run("pending operation with a disposition", func(t *testing.T) {
		s := base()
		problems := bearerDispositionProblems(s, map[OperationID]bool{s.ID: true})
		if len(problems) == 0 {
			t.Error("a pending operation that declares a disposition must be rejected")
		}
	})
}

// TestBearerDisposition_CredentialsMatchDisposition requires every catalog
// operation with a recorded disposition to list scoped_uat in Credentials
// if and only if the disposition admits a token (admit or admit_self).
func TestBearerDisposition_CredentialsMatchDisposition(t *testing.T) {
	pending := pendingOperationSet()
	for _, spec := range Catalog {
		if pending[spec.ID] {
			continue
		}
		hasUAT := false
		for _, c := range spec.Credentials {
			if c == CredentialScopedUAT {
				hasUAT = true
			}
		}
		if spec.Bearer.Kind.AdmitsToken() != hasUAT {
			t.Errorf("operation %s: bearer kind %q but scoped_uat listed = %v", spec.ID, spec.Bearer.Kind, hasUAT)
		}
	}

	// Validate enforces the same rule on any spec.
	admitWithoutUAT := validSpec()
	admitWithoutUAT.Bearer = AdmitOn(BearerTargetProjectPath, BearerBoundaryProject)
	if err := admitWithoutUAT.Validate(); err == nil || !strings.Contains(err.Error(), "must list scoped_uat") {
		t.Errorf("admit without scoped_uat must fail Validate, got %v", err)
	}
	sessionWithUAT := validSpec()
	sessionWithUAT.Credentials = append(sessionWithUAT.Credentials, CredentialScopedUAT)
	if err := sessionWithUAT.Validate(); err == nil || !strings.Contains(err.Error(), "must not list scoped_uat") {
		t.Errorf("session_only with scoped_uat must fail Validate, got %v", err)
	}
	nonUserWithUAT := validSpec()
	nonUserWithUAT.Bearer = NonUser()
	nonUserWithUAT.Credentials = []CredentialKind{CredentialScopedUAT}
	if err := nonUserWithUAT.Validate(); err == nil || !strings.Contains(err.Error(), "must not list scoped_uat") {
		t.Errorf("non_user with scoped_uat must fail Validate, got %v", err)
	}
}
