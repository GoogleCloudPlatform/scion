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
	markBrokerRuntimeProfile(t, f, "docker")
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

// TestScheduledDispatch_HubDefaultPassthroughAppliedOnStockEmbeddedBroker is
// the scheduled-dispatch regression test for the stock single-node VM shape,
// mirroring TestHubDefaultGCPIdentity_PassthroughAppliedOnStockEmbeddedBroker:
// the embedded broker reports the real two-profile set (local=docker,
// remote=kubernetes) with DefaultProfile "local", and the scheduled dispatch
// (which never names a profile) must still resolve to the broker's own
// default and get passthrough, with that profile pinned onto
// AppliedConfig.Profile.
func TestScheduledDispatch_HubDefaultPassthroughAppliedOnStockEmbeddedBroker(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	markBrokerStockProfiles(t, f, "local")
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-hub-passthrough-stock"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-hub-passthrough-stock")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModePassthrough, got.AppliedConfig.GCPIdentity.MetadataMode,
		"the stock embedded broker's own default profile (docker) must still get the hub default on scheduled dispatch")
	assert.Equal(t, "local", got.AppliedConfig.Profile,
		"the resolved profile must be pinned so dispatch cannot use a different one than the gate checked")
}

// TestScheduledDispatch_HubDefaultPassthroughDeniedOnKubernetesRuntimeLeavesIdentityUnset
// mirrors
// TestHubDefaultGCPIdentity_PassthroughDeniedOnKubernetesRuntimeLeavesIdentityUnset:
// the runtime-aware gate applies identically on the scheduled dispatch path,
// since both surfaces route through hubDefaultPassthroughAllowed. The
// embedded broker qualifies, but its resolved runtime profile is
// kubernetes-type, so the hub-default grant is denied — and per
// ptone/scion#2328, that denial leaves GCPIdentity unset rather than writing
// an explicit "block", so the broker's own runtime-aware default
// (passthrough, for Kubernetes) applies instead.
func TestScheduledDispatch_HubDefaultPassthroughDeniedOnKubernetesRuntimeLeavesIdentityUnset(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	markBrokerRuntimeProfile(t, f, "kubernetes")
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-hub-passthrough-k8s"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-hub-passthrough-k8s")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	assert.Nil(t, got.AppliedConfig.GCPIdentity,
		"hub-default passthrough denied on a scheduled dispatch to a kubernetes-type runtime profile must leave "+
			"GCPIdentity unset, not write an explicit block (ptone/scion#2328)")
}

// TestScheduledDispatch_HubDefaultPassthroughNotAppliedOnNonEmbeddedBroker
// mirrors TestHubDefaultGCPIdentity_PassthroughNotAppliedOnNonEmbeddedBroker:
// on a scheduled dispatch to a broker that is not the hub's embedded broker
// (here, no embedded broker registered at all), the hub-default passthrough
// must not apply. As of ptone/scion#2328, the denial leaves GCPIdentity
// unset rather than writing an explicit "block": the operator
// chose "passthrough", not "block", so this is treated exactly like no hub
// default at all, letting the broker apply its own runtime-aware default
// (unchanged "block" on every runtime except Kubernetes) instead of exposing
// that broker's host identity via an unchecked passthrough.
func TestScheduledDispatch_HubDefaultPassthroughNotAppliedOnNonEmbeddedBroker(t *testing.T) {
	f := bypassAgentsSetup(t)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-hub-passthrough-remote"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-hub-passthrough-remote")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	assert.Nil(t, got.AppliedConfig.GCPIdentity,
		"hub-default passthrough denied by the embedded-broker gate must leave GCPIdentity unset, not write an explicit block")
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
// review sg-rev.md: the `case store.GCPMetadataModeBlock:` arm in
// applyScheduledProjectDefaultGCPIdentity (server.go) is the only thing
// stopping a project's explicit Block from falling through to the hub
// default now that the default arm consults it. Before this PR, "block" and
// "unset" behaved identically on the scheduled path because nothing followed
// either; this test pins that an explicit project Block still does not fall
// through, even though a hub default that would otherwise apply (passthrough
// on the embedded broker) is configured. Mirrors the HTTP path's
// TestHubDefaultGCPIdentity_ProjectExplicitBlockWinsOverHubPassthrough.
//
// ptone/scion#2328: this arm now writes an explicit "block" record instead
// of leaving GCPIdentity nil (previously nil was this path's representation
// of both "block" and "nothing configured"). It has to, now that nil means
// "apply the broker's runtime-aware default": an explicit project Block
// dispatched to the Kubernetes runtime must still be rejected by the broker
// (pkg/runtimebroker/start_context_test.go), not silently become
// "passthrough" because it looked the same as "nothing configured".
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
	require.NotNil(t, got.AppliedConfig.GCPIdentity,
		"an explicit project Block must be written as an explicit record, not left nil, "+
			"or it becomes indistinguishable from \"nothing configured\"")
	assert.Equal(t, store.GCPMetadataModeBlock, got.AppliedConfig.GCPIdentity.MetadataMode,
		"an explicit project Block must not fall through to a hub default that would otherwise apply")
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
	require.NotNil(t, got.AppliedConfig.GCPIdentity,
		"an explicit project Block is written as an explicit record (ptone/scion#2328 phase 1), not left nil")
	assert.Equal(t, store.GCPMetadataModeBlock, got.AppliedConfig.GCPIdentity.MetadataMode)
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
	assert.Nil(t, got.AppliedConfig.GCPIdentity,
		"hub-default passthrough denied because the project's own broker is not the hub's embedded broker "+
			"must leave GCPIdentity unset, not write an explicit block (ptone/scion#2328)")
}

// TestScheduledDispatch_NoHubDefaultLeavesGCPIdentityUnchanged double-checks,
// from this feature's side, the invariant
// TestScheduledDispatch_NoProjectDefaultLeavesGCPIdentityUnchanged already
// pins: with neither a project default nor a hub default configured, the
// scheduler path's prior behaviour is unchanged — AppliedConfig.GCPIdentity
// stays nil. As of ptone/scion#2328 phase 1 the create path's floor now
// matches this (see TestHubDefaultGCPIdentity_NoDefaultsLeavesGCPIdentityUnset)
// instead of writing an explicit "block" record, so the two paths agree: nil
// means "let the broker apply its own runtime-aware default".
func TestScheduledDispatch_NoHubDefaultLeavesGCPIdentityUnchanged(t *testing.T) {
	f := bypassAgentsSetup(t)

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-no-hub-default"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-no-hub-default")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	assert.Nil(t, got.AppliedConfig.GCPIdentity,
		"with no project default and no hub default the scheduler path must keep its prior behaviour")
}

// TestScheduledDispatch_HubDefaultExplicitlyBlockIsExplicit is the scheduled
// path's twin of TestHubDefaultGCPIdentity_HubDefaultExplicitlyBlockIsExplicit:
// a hub operator who configures "block" as the hub-wide default has made an
// explicit choice, distinct from "nothing configured"
// (TestScheduledDispatch_NoHubDefaultLeavesGCPIdentityUnchanged), and it must
// stay an explicit record rather than being silently reinterpreted as "apply
// the broker's runtime-aware default" on a Kubernetes-bound dispatch.
func TestScheduledDispatch_HubDefaultExplicitlyBlockIsExplicit(t *testing.T) {
	f := bypassAgentsSetup(t)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModeBlock,
	})

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-hub-default-explicit-block"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-hub-default-explicit-block")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity,
		"an explicit hub-default Block must be written as an explicit record, not left nil")
	assert.Equal(t, store.GCPMetadataModeBlock, got.AppliedConfig.GCPIdentity.MetadataMode)
}

// Note: this file does not additionally pin a Kubernetes-broker-specific
// scenario for the scheduled path. The resolution ladder above is
// runtime-agnostic — it never reads a broker's profile type — so a
// Kubernetes-flavored variant of the tests above would duplicate them without
// exercising anything Kubernetes-specific. What the broker actually does with
// a nil vs. an explicit "block" GCPIdentity (passthrough default vs.
// rejection) is covered end to end by pkg/runtimebroker/start_context_test.go
// (TestBuildStartContext_GCPMetadataNoIdentityInputOnKubernetesDefaultsToPassthrough
// and TestBuildStartContext_GCPMetadataBlockRejectedOnKubernetes).

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

// TestScheduledDispatch_HubDefaultPassthroughDeniedByProjectActiveProfileLeavesIdentityUnset
// mirrors TestHubDefaultGCPIdentity_PassthroughDeniedByProjectActiveProfileOnStockBrokerLeavesIdentityUnset
// on the scheduled dispatch path: the project's active-profile setting must
// be what the gate evaluates there too, not the broker's own default
// profile, even though a scheduled dispatch never names a profile itself.
// Per ptone/scion#2328, the denial leaves GCPIdentity unset rather than
// writing an explicit "block".
func TestScheduledDispatch_HubDefaultPassthroughDeniedByProjectActiveProfileLeavesIdentityUnset(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	markBrokerStockProfiles(t, f, "local") // broker default points at docker
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingActiveProfile] = "remote"
	require.NoError(t, f.store.UpdateProject(ctx, proj))

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-hub-passthrough-project-remote"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-hub-passthrough-project-remote")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	assert.Nil(t, got.AppliedConfig.GCPIdentity,
		"the project's active profile (remote/kubernetes) must be what the gate evaluates on scheduled dispatch, "+
			"not the broker's own default, and the denial must leave GCPIdentity unset (ptone/scion#2328)")
}

// TestScheduledDispatch_PinnedProfileSurvivesReincarnateAfterProjectActiveProfileChanges
// is the scheduled-dispatch twin of
// TestHubDefaultGCPIdentity_PinnedProfileSurvivesReincarnateAfterProjectActiveProfileChanges:
// the pin lands on CreateInputs.Profile on this path too, and survives a
// project active-profile change made after the grant.
func TestScheduledDispatch_PinnedProfileSurvivesReincarnateAfterProjectActiveProfileChanges(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	markBrokerStockProfiles(t, f, "local")
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-hub-passthrough-reincarnate"))

	ctx := context.Background()
	agent, err := f.store.GetAgentBySlug(ctx, f.proj.ID, "sched-hub-passthrough-reincarnate")
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	require.NotNil(t, agent.AppliedConfig.GCPIdentity)
	require.Equal(t, store.GCPMetadataModePassthrough, agent.AppliedConfig.GCPIdentity.MetadataMode)
	require.Equal(t, "local", agent.AppliedConfig.Profile)
	require.NotNil(t, agent.AppliedConfig.CreateInputs)
	require.Equal(t, "local", agent.AppliedConfig.CreateInputs.Profile,
		"the pin must also reach CreateInputs.Profile on the scheduled path, or reincarnate loses it")

	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingActiveProfile] = "remote"
	require.NoError(t, f.store.UpdateProject(ctx, proj))
	proj, err = f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)

	fresh, _, err := f.srv.buildFreshAppliedConfig(ctx, agent, proj, "")
	require.NoError(t, err)
	assert.Equal(t, "local", fresh.Profile,
		"reincarnate must replay the pinned profile from CreateInputs, not re-derive the project's current active profile")
}
