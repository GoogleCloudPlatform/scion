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

// CO1 cutover: All policy handler tests have been removed.
// The Policy API now returns 410 Gone. The request/response types
// (CreatePolicyRequest, UpdatePolicyRequest) have been retired.
// See handlers_permissions_test.go for 410 verification tests.

import "testing"

func TestCreatePolicy_SourceIPsRejection(t *testing.T) {
	// CO1: Policy API retired. See handlers_permissions_test.go TestPolicyCreate.
}

func TestUpdatePolicy_SourceIPsRejection(t *testing.T) {
	// CO1: Policy API retired. See handlers_permissions_test.go TestPolicyUpdate.
}

func TestUpdatePolicy_PolicyKindPreservation(t *testing.T) {
	// CO1: Policy API retired. PolicyKind is no longer relevant.
}
