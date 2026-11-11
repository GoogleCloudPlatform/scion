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

package permissions

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompatibilityCeilingV1IsPinned is a literal golden of every V1 row. It
// deliberately does not compare against live role coverage: the table must
// not follow later Registry or role-scope changes.
func TestCompatibilityCeilingV1IsPinned(t *testing.T) {
	readonly := []string{
		"harness_config.list", "harness_config.read", "project.read",
		"skill.list", "skill.read", "template.list", "template.read",
	}
	baseline := []string{
		"agent.notify", "agent.port_forward", "agent.status_update", "agent.token_refresh",
		"harness_config.list", "harness_config.read", "project.read",
		"skill.list", "skill.read", "template.list", "template.read",
	}
	full := []string{
		"agent.attach", "agent.create", "agent.delete", "agent.lifecycle",
		"agent.notify", "agent.port_forward", "agent.set_message_mode",
		"agent.status_update", "agent.token_refresh", "env_var.deliver",
		"gcp_service_account.assign", "gcp_service_account.use",
		"harness_config.list", "harness_config.read", "project.read",
		"project.secret_read", "secret.deliver", "secret.use",
		"skill.list", "skill.read", "skill_injection.deliver",
		"template.create", "template.list", "template.read", "template.update",
	}
	require.Len(t, compatibilityCeilingV1, 3)
	assert.Equal(t, readonly, compatibilityCeilingV1["readonly"])
	assert.Equal(t, baseline, compatibilityCeilingV1["baseline"])
	assert.Equal(t, full, compatibilityCeilingV1["full"])
	assert.Equal(t, []string{"gcp_service_account.assign", "gcp_service_account.use"}, compatibilityAssignedSAV1)

	for _, role := range CompatibilityRoles() {
		ids, ok := CompatibilityCeiling(CompatibilityPolicyV1, role, false)
		require.True(t, ok, role)
		assert.Equal(t, compatibilityCeilingV1[role], ids, role)
	}
	withSA, ok := CompatibilityCeiling(CompatibilityPolicyV1, "readonly", true)
	require.True(t, ok)
	assert.Equal(t, []string{
		"gcp_service_account.assign", "gcp_service_account.use",
		"harness_config.list", "harness_config.read", "project.read",
		"skill.list", "skill.read", "template.list", "template.read",
	}, withSA)
	fullSA, ok := CompatibilityCeiling(CompatibilityPolicyV1, "full", true)
	require.True(t, ok)
	assert.Equal(t, full, fullSA, "full already carries the assigned-SA permissions")
}

func TestCompatibilityCeilingV1IDsAreRegistered(t *testing.T) {
	registered := make(map[string]bool, len(Registry))
	for _, p := range Registry {
		registered[p.ID] = true
	}
	for role, ids := range compatibilityCeilingV1 {
		for _, id := range ids {
			assert.True(t, registered[id], "%s: %s is not a registered permission", role, id)
		}
	}
	for _, id := range compatibilityAssignedSAV1 {
		assert.True(t, registered[id], id)
	}
}

func TestCompatibilityCeilingRejectsUnknownVersionAndRole(t *testing.T) {
	for _, v := range []CompatibilityPolicyVersion{0, 2, -1} {
		ids, ok := CompatibilityCeiling(v, "full", false)
		assert.False(t, ok, "version %d", v)
		assert.Nil(t, ids)
	}
	for _, role := range []string{"", "none", "admin", "FULL"} {
		ids, ok := CompatibilityCeiling(CompatibilityPolicyV1, role, true)
		assert.False(t, ok, "role %q", role)
		assert.Nil(t, ids)
	}
}

func TestCompatibilityCeilingV1ExcludesIdentityToken(t *testing.T) {
	for _, role := range CompatibilityRoles() {
		for _, sa := range []bool{false, true} {
			ids, ok := CompatibilityCeiling(CompatibilityPolicyV1, role, sa)
			require.True(t, ok)
			assert.NotContains(t, ids, "agent.identity_token", role)
		}
	}
}

// The returned slice is a copy: editing it does not change the table.
func TestCompatibilityCeilingReturnsCopy(t *testing.T) {
	ids, ok := CompatibilityCeiling(CompatibilityPolicyV1, "readonly", false)
	require.True(t, ok)
	ids[0] = "agent.identity_token"
	again, _ := CompatibilityCeiling(CompatibilityPolicyV1, "readonly", false)
	assert.Equal(t, "harness_config.list", again[0])
}
