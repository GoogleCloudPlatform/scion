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

package hub

import (
	"time"
)

// testNow is a fixed time used across all kernel tests for determinism.
var testNow = time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)

func makeRole(id, name, scopeType string, perms ...string) *RolePermissions {
	return NewRolePermissions(id, name, scopeType, perms)
}

func makeBinding(bindingID, roleDefID, principalType, principalID, scopeType, scopeID string) CandidateBinding {
	return CandidateBinding{
		BindingID:        bindingID,
		RoleDefinitionID: roleDefID,
		PrincipalType:    principalType,
		PrincipalID:      principalID,
		ScopeType:        scopeType,
		ScopeID:          scopeID,
	}
}

func makeTimedBinding(bindingID, roleDefID, principalType, principalID, scopeType, scopeID string, notBefore, expiresAt time.Time) CandidateBinding {
	cb := makeBinding(bindingID, roleDefID, principalType, principalID, scopeType, scopeID)
	cb.NotBefore = notBefore
	cb.ExpiresAt = expiresAt
	return cb
}

func closureOf(ids ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}
