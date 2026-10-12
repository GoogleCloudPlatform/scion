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

import (
	"context"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The execution-project stage resolves its source user through the recorded
// provenance chain unless a test injects a resolver.
func TestProvenanceRootSourceResolverInstalled(t *testing.T) {
	_, s := authzTestSetup(t)
	a := NewAuthzService(s, slog.Default())
	r, ok := a.executionSourceResolver().(provenanceRootSourceResolver)
	require.True(t, ok, "resolver is %T", a.executionSourceResolver())
	assert.Same(t, a, r.a)

	stub := stubSourceResolver{}
	a.sourceResolver = stub
	assert.Equal(t, stub, a.executionSourceResolver())
}

// executionProjectAdmission keeps its outcomes and reason strings with the
// provenance-root resolver, including for an unrecorded edge, which the
// stage accepts as a source.
func TestExecutionAdmissionUnchangedAfterSwap(t *testing.T) {
	const noSource = "execution agent has no authoritative source user"
	type tc struct {
		name   string
		setup  func(t *testing.T, f provenanceFixture) (agentID, tokenProject string)
		ok     bool
		reason string
	}
	cases := []tc{
		{"user root admitted", func(t *testing.T, f provenanceFixture) (string, string) {
			return f.userChild(t, "swap-ok", ceilPrincip).ID, f.projectID
		}, true, ""},
		{"agent chain", func(t *testing.T, f provenanceFixture) (string, string) {
			p := f.userChild(t, "swap-chain-p", ceilPrincip)
			return f.agentChild(t, "swap-chain-c", p, boundedCeiling("skill.read")).ID, f.projectID
		}, true, ""},
		{"unrecorded edge", func(t *testing.T, f provenanceFixture) (string, string) {
			ag := f.agent(t, "swap-unrec", AgentRoleFull)
			f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
			return ag.ID, f.projectID
		}, true, ""},
		{"missing edge", func(t *testing.T, f provenanceFixture) (string, string) {
			return f.agent(t, "swap-missing", AgentRoleFull).ID, f.projectID
		}, false, noSource},
		{"mismatched edge", func(t *testing.T, f provenanceFixture) (string, string) {
			ag := f.agent(t, "swap-mismatch", AgentRoleFull)
			f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, ceilPrincip, recordedProv(store.DelegationPrincipalAgent, f.userID, store.SourceCredentialSession))
			return ag.ID, f.projectID
		}, false, noSource},
		{"migration sentinel", func(t *testing.T, f provenanceFixture) (string, string) {
			ag := f.agent(t, "swap-sentinel", AgentRoleFull)
			f.edge(t, store.DelegationPrincipalUser, migrationDelegatorID, ag.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
			return ag.ID, f.projectID
		}, false, noSource},
		{"soft-deleted intermediate", func(t *testing.T, f provenanceFixture) (string, string) {
			p := f.userChild(t, "swap-del-p", ceilPrincip)
			c := f.agentChild(t, "swap-del-c", p, boundedCeiling("skill.read"))
			softDeleteStoredAgent(t, f.store, p.ID)
			return c.ID, f.projectID
		}, false, noSource},
		{"inactive source", func(t *testing.T, f provenanceFixture) (string, string) {
			ag := f.userChild(t, "swap-inactive", ceilPrincip)
			setUserStatus(t, f.store, f.userID, store.UserStatusSuspended)
			return ag.ID, f.projectID
		}, false, "execution source user is not active"},
		{"source not admitted", func(t *testing.T, f provenanceFixture) (string, string) {
			outsider := tid("swap-outsider")
			other := tid("swap-outsider-proj")
			createDCProject(t, f.store, other, "swap-outsider-proj")
			createDCUser(t, f.store, outsider, "swap-outsider@test.com", other, store.ProjectRoleOwner)
			ag := f.agent(t, "swap-outsider", AgentRoleFull)
			f.edge(t, store.DelegationPrincipalUser, outsider, ag.ID, ceilPrincip, recordedProv(store.DelegationPrincipalUser, outsider, store.SourceCredentialSession))
			return ag.ID, f.projectID
		}, false, "execution source user lacks admission to the agent's project"},
		{"deleted agent", func(t *testing.T, f provenanceFixture) (string, string) {
			ag := f.userChild(t, "swap-deleted", ceilPrincip)
			softDeleteStoredAgent(t, f.store, ag.ID)
			return ag.ID, f.projectID
		}, false, "execution agent is deleted"},
		{"token project mismatch", func(t *testing.T, f provenanceFixture) (string, string) {
			return f.userChild(t, "swap-xproj", ceilPrincip).ID, tid("swap-other-project")
		}, false, "execution project does not match the agent's project"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newProvenanceFixture(t, "swap-"+string(rune('a'+i)), false)
			agentID, tokenProject := c.setup(t, f)
			ok, reason := f.a.executionProjectAdmission(context.Background(), PrincipalContext{
				Kind:     PrincipalKindAgent,
				ID:       agentID,
				Identity: dcAgentIdentity(agentID, tokenProject, AgentRoleFull),
			}, "skill.read")
			assert.Equal(t, c.ok, ok, "reason %q", reason)
			assert.Equal(t, c.reason, reason)
		})
	}
}
