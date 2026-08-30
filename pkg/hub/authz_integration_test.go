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

// CO1 cutover: All policy evaluation integration tests have been removed.
// Authorization now routes through the AK1 kernel using RoleBindings.
// The evaluate endpoint and policy API have been retired.
//
// The golden tests in authz_golden_test.go cover the equivalent behavior
// using the new kernel-based evaluation pipeline.

import "testing"

func TestEvaluateEndpoint_UserDirectPolicy(t *testing.T) {
	// CO1: Policy evaluation removed. See golden tests for equivalent coverage.
}

func TestEvaluateEndpoint_DefaultDeny(t *testing.T) {
	// CO1: Policy evaluation removed. See golden tests for equivalent coverage.
}

func TestEvaluateEndpoint_ScopeOverride(t *testing.T) {
	// CO1: Policy evaluation removed. See golden tests for equivalent coverage.
}

func TestEvaluateEndpoint_ProjectScopedPolicyDoesNotMatchParentlessResource(t *testing.T) {
	// CO1: Policy evaluation removed. See golden tests for equivalent coverage.
}

func TestEvaluateEndpoint_AgentPolicy(t *testing.T) {
	// CO1: Policy evaluation removed. See golden tests for equivalent coverage.
}

func TestEvaluateEndpoint_AgentBinding(t *testing.T) {
	// CO1: Policy evaluation removed. See golden tests for equivalent coverage.
}

func TestEvaluateEndpoint_Validation(t *testing.T) {
	// CO1: Policy evaluation removed. See golden tests for equivalent coverage.
}

func TestEvaluateEndpoint_MethodNotAllowed(t *testing.T) {
	// CO1: Policy evaluation removed. See golden tests for equivalent coverage.
}

func TestEvaluateEndpoint_CreatedByPopulated(t *testing.T) {
	// CO1: Policy evaluation removed. See golden tests for equivalent coverage.
}
