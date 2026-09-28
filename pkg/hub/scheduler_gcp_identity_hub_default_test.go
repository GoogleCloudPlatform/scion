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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// ptone/scion#1927: applyScheduledProjectDefaultGCPIdentity (server.go) gains
// the same hub-default rung that createAgentInProject already has
// (handlers_agents_core.go, ptone/scion#1906). These tests are the scheduled
// dispatch twin of hub_gcp_identity_default_test.go, using the scheduler
// fixtures and helpers from scheduler_creator_identity_test.go.
// =============================================================================

// TestScheduledDispatch_HubDefaultPassthroughAppliedWhenNoProjectDefault
// mirrors TestHubDefaultGCPIdentity_PassthroughAppliedWhenNoProjectDefault: a
// hub-wide passthrough default reaches an agent dispatched by a schedule on a
// project with no project-level override, on the embedded broker.
func TestScheduledDispatch_HubDefaultPassthroughAppliedWhenNoProjectDefault(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-hub-passthrough"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-hub-passthrough")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModePassthrough, got.AppliedConfig.GCPIdentity.MetadataMode)
}

// TestScheduledDispatch_HubDefaultPassthroughNotAppliedOnNonEmbeddedBroker
// mirrors TestHubDefaultGCPIdentity_PassthroughNotAppliedOnNonEmbeddedBroker:
// on a scheduled dispatch to a broker that is not the hub's embedded broker
// (here, no embedded broker registered at all), the hub-default passthrough
// must not apply and the dispatch falls back to block rather than exposing
// that broker's host identity.
func TestScheduledDispatch_HubDefaultPassthroughNotAppliedOnNonEmbeddedBroker(t *testing.T) {
	f := bypassAgentsSetup(t)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-hub-passthrough-remote"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-hub-passthrough-remote")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity,
		"hub-default passthrough denied by the embedded-broker gate must still write an explicit block, not leave the field nil")
	assert.Equal(t, store.GCPMetadataModeBlock, got.AppliedConfig.GCPIdentity.MetadataMode,
		"hub-default passthrough must not apply on a non-embedded broker")
}

// setProjectDefaultGCPMode sets the project's default GCP identity mode
// annotation directly (no service account), for tests that only need to
// exercise a mode value — passthrough or block — rather than an assign
// target. setProjectDefaultSAAnnotations (scheduler_creator_identity_test.go)
// covers the assign+SA case; this is its mode-only counterpart.
func setProjectDefaultGCPMode(t *testing.T, f *bypassAgentsFixture, mode string) {
	t.Helper()
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = mode
	require.NoError(t, f.store.UpdateProject(ctx, proj))
}

// TestScheduledDispatch_ProjectDefaultWinsOverHubDefault mirrors
// TestHubDefaultGCPIdentity_ProjectDefaultTakesPrecedenceOverHubDefault: a
// project default answers the question before the hub default is ever
// consulted on the scheduled path too, regardless of what the hub default
// would have produced (here, an unusable SA ID that would fail dispatch if it
// were ever looked up).
func TestScheduledDispatch_ProjectDefaultWinsOverHubDefault(t *testing.T) {
	f := bypassAgentsSetup(t)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: "does-not-exist",
	})

	setProjectDefaultGCPMode(t, f, store.GCPMetadataModePassthrough)

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-project-over-hub"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-project-over-hub")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModePassthrough, got.AppliedConfig.GCPIdentity.MetadataMode,
		"the project default must win, and the hub default's unusable SA must never be looked up")
}

// TestScheduledDispatch_ProjectBlockNotOverriddenByHubDefault covers R1 from
// review sg-rev.md: the new `case store.GCPMetadataModeBlock:` arm in
// applyScheduledProjectDefaultGCPIdentity (server.go) is the only thing
// stopping a project's explicit Block from falling through to the hub
// default now that the default arm consults it. Before this PR, "block" and
// "unset" behaved identically on the scheduled path because nothing followed
// either; this test pins that an explicit project Block still does not fall
// through, even though a hub default that would otherwise apply (passthrough
// on the embedded broker) is configured. Mirrors the HTTP path's
// TestHubDefaultGCPIdentity_ProjectExplicitBlockWinsOverHubPassthrough.
func TestScheduledDispatch_ProjectBlockNotOverriddenByHubDefault(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	setProjectDefaultGCPMode(t, f, store.GCPMetadataModeBlock)

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-project-block-wins"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-project-block-wins")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	assert.Nil(t, got.AppliedConfig.GCPIdentity,
		"an explicit project Block must not fall through to a hub default that would otherwise apply; "+
			"nil is the scheduled path's pre-existing representation of block for this mode")
}

// TestScheduledDispatch_ProjectBlockStopsBeforeHubDefaultAssignLookup is the
// review's optional second variant of R1: a hub default naming an
// unresolvable SA must never be looked up when the project explicitly set
// Block, because the ladder stops at the project rung. If the block arm ever
// regressed into consulting the hub default, this dispatch would fail
// (the SA does not exist) instead of succeeding with no GCP identity.
func TestScheduledDispatch_ProjectBlockStopsBeforeHubDefaultAssignLookup(t *testing.T) {
	f := bypassAgentsSetup(t)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: "does-not-exist",
	})

	setProjectDefaultGCPMode(t, f, store.GCPMetadataModeBlock)

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-project-block-stops-assign-lookup"),
		"the hub default's unresolvable SA must never be looked up when the project explicitly blocked")

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-project-block-stops-assign-lookup")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	assert.Nil(t, got.AppliedConfig.GCPIdentity)
}

// TestScheduledDispatch_HubDefaultAssignEmptySAIDFallsBackToBlock covers O1
// from review sg-rev.md: a hub default configured as "assign" but with no
// service account ID selected (server.go ~3626-3631) must fail safe to an
// explicit block, not be skipped or treated as unset, exactly as the
// equivalent project-default and HTTP hub-default arms already do.
func TestScheduledDispatch_HubDefaultAssignEmptySAIDFallsBackToBlock(t *testing.T) {
	f := bypassAgentsSetup(t)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: "",
	})

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-hub-default-assign-no-sa"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-hub-default-assign-no-sa")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity,
		"a misconfigured hub-default assign (no SA selected) must write an explicit block, not leave the field nil")
	assert.Equal(t, store.GCPMetadataModeBlock, got.AppliedConfig.GCPIdentity.MetadataMode)
}

// TestScheduledDispatch_HubDefaultPassthroughNotAppliedWhenProjectUsesADifferentBroker
// covers O2 from review sg-rev.md: the more production-relevant passthrough
// gate case is not "no embedded broker at all" (already covered by
// TestScheduledDispatch_HubDefaultPassthroughNotAppliedOnNonEmbeddedBroker)
// but an embedded broker that IS registered while the project's own runtime
// provider points at a different broker. hubDefaultPassthroughAllowed must
// still deny, because the agent is not actually dispatched to the embedded
// broker (mirrors the HTTP path's
// TestHubDefaultGCPIdentity_PassthroughNotAppliedOnSpoofedEmbeddedLabel,
// which pins the same gate function against a broker that merely claims to
// be embedded via its label).
func TestScheduledDispatch_HubDefaultPassthroughNotAppliedWhenProjectUsesADifferentBroker(t *testing.T) {
	f := bypassAgentsSetup(t)
	// The hub does have an embedded broker — just not the one this project
	// dispatches to (f.broker, wired by bypassAgentsSetup's AddProjectProvider).
	f.srv.SetEmbeddedBrokerID("some-other-embedded-broker")
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-hub-passthrough-wrong-broker"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-hub-passthrough-wrong-broker")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeBlock, got.AppliedConfig.GCPIdentity.MetadataMode,
		"hub-default passthrough must not apply when the project's own broker is not the hub's embedded broker")
}

// TestScheduledDispatch_NoHubDefaultLeavesGCPIdentityUnchanged double-checks,
// from this feature's side, the invariant
// TestScheduledDispatch_NoProjectDefaultLeavesGCPIdentityUnchanged already
// pins: with neither a project default nor a hub default configured, the
// scheduler path's prior behaviour is unchanged — AppliedConfig.GCPIdentity
// stays nil, not an explicit "block" record (unlike the create path's floor).
func TestScheduledDispatch_NoHubDefaultLeavesGCPIdentityUnchanged(t *testing.T) {
	f := bypassAgentsSetup(t)

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-no-hub-default"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-no-hub-default")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	assert.Nil(t, got.AppliedConfig.GCPIdentity,
		"with no project default and no hub default the scheduler path must keep its prior behaviour")
}

// TestScheduledDispatch_HubDefaultAssignSameAuthorizationAsProjectDefault
// mirrors TestScheduledDispatch_ProjectDefaultSAAssigned one rung down the
// ladder: a hub-default "assign" on a scheduled dispatch runs through the
// same evaluateSAAssignment gate, against the same principal (the schedule's
// immediate creator) as the project-default rung already uses on this path,
// and is recorded under the hub-default audit surface.
func TestScheduledDispatch_HubDefaultAssignSameAuthorizationAsProjectDefault(t *testing.T) {
	f := bypassAgentsSetup(t)
	audit := &mockAuditLogger{}
	f.srv.SetAuditLogger(audit)

	sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: sa.ID,
	})
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().AllowTarget(sa.Email))

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-hub-default-sa"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-hub-default-sa")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeAssign, got.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Equal(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID)
	assert.Equal(t, sa.Email, got.AppliedConfig.GCPIdentity.ServiceAccountEmail)

	ev := onlySAEvent(t, audit)
	assert.Equal(t, SurfaceHubDefault, ev.Surface,
		"a hub-default assignment on the scheduled path must be labelled as hub-default, not project-default")
}

// TestScheduledDispatch_HubDefaultAssignDeniedFailsDispatch mirrors
// TestScheduledDispatch_ProjectDefaultSADeniedFailsDispatch one rung down:
// a hub default naming an SA the schedule's creator cannot act as must fail
// the dispatch rather than silently falling back to block.
func TestScheduledDispatch_HubDefaultAssignDeniedFailsDispatch(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: sa.ID,
	})
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().DenyTarget(sa.Email, "no actAs grant"))

	err := fireScheduledDispatchAsOwner(t, f, "sched-hub-default-denied-sa")
	require.Error(t, err, "a creator without actAs on the hub-default SA must not get a scheduled agent")
	assert.Contains(t, err.Error(), store.PermissionActAs, "denial must come from the actAs gate")

	_, getErr := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-hub-default-denied-sa")
	assert.ErrorIs(t, getErr, store.ErrNotFound, "denied dispatch must not create the agent record")
}
