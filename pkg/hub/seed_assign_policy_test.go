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

//go:build !no_sqlite

package hub

// CO1 cutover: Per-project assign policy tests removed.
// The assign policy functions (projectAssignPolicyName, ensureProjectAssignPolicy,
// backfillProjectAssignPolicies) are no-ops after cutover. The policy matching
// functions (matchesResource, matchesAction) have been removed.
// Authorization now routes through the AK1 kernel using RoleBindings.

import "testing"

func TestProjectAssignPolicy_Shape(t *testing.T) {
	// CO1: Policy API and assign policies retired.
}

func TestProjectAssignPolicy_BoundToMembersGroup(t *testing.T) {
	// CO1: Policy API and assign policies retired.
}

func TestProjectAssignPolicy_CannotReachHubScopedSA(t *testing.T) {
	// CO1: Policy API and assign policies retired.
}

func TestProjectAssignPolicy_Idempotent(t *testing.T) {
	// CO1: Policy API and assign policies retired.
}

func TestBackfillProjectAssignPolicies(t *testing.T) {
	// CO1: Policy API and assign policies retired.
}

func TestBackfillProjectAssignPolicies_Idempotent(t *testing.T) {
	// CO1: Policy API and assign policies retired.
}

func TestBackfillProjectAssignPolicies_SkipsGrouplessProject(t *testing.T) {
	// CO1: Policy API and assign policies retired.
}

func TestBackfillProjectAssignPolicies_MultipleProjects(t *testing.T) {
	// CO1: Policy API and assign policies retired.
}
