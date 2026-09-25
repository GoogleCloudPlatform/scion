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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Hub-level default GCP identity mode (ptone/scion#1857).
//
// Fallback ladder for GCP identity, from highest to lowest precedence:
//
//	explicit create request -> project default -> hub default -> block
//
// These tests cover the newly-added hub-default rung in
// handlers_agents_core.go, one below the existing project-default rung
// covered by project_default_gate_test.go and handlers_agents_gcp_hubscope_test.go.
// =============================================================================

// TestHubDefaultGCPIdentity_NoDefaultsFallBackToBlock is the baseline: with
// neither a project default nor a hub default configured, agent creation
// still lands on "block" — the ladder's floor is unchanged by this feature.
func TestHubDefaultGCPIdentity_NoDefaultsFallBackToBlock(t *testing.T) {
	f := bypassAgentsSetup(t)

	identity := createdAgentIdentity(t, f, "no-defaults-agent")
	assert.Equal(t, store.GCPMetadataModeBlock, identity.MetadataMode)
}

// markBrokerEmbedded records the fixture's broker as the hub's embedded
// (co-located) broker — the one a single-node VM dispatches to — the same way
// server startup does, via the server-held embedded broker ID.
func markBrokerEmbedded(t *testing.T, f *bypassAgentsFixture) {
	t.Helper()
	f.srv.SetEmbeddedBrokerID(f.broker.ID)
}

// TestHubDefaultGCPIdentity_PassthroughAppliedWhenNoProjectDefault covers the
// motivating case from the brief: a single-node VM admin sets "passthrough"
// as the hub-wide default, and a project with no default of its own inherits
// it on the embedded broker.
func TestHubDefaultGCPIdentity_PassthroughAppliedWhenNoProjectDefault(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	identity := createdAgentIdentity(t, f, "hub-passthrough-agent")
	assert.Equal(t, store.GCPMetadataModePassthrough, identity.MetadataMode)
}

// TestHubDefaultGCPIdentity_PassthroughNotAppliedOnNonEmbeddedBroker pins
// review R1: hub-default passthrough skips the broker-owner/actAs gate that
// explicit passthrough requests go through, so it is confined to the embedded
// broker. On any other broker — here a remote auto-provide broker — the
// ladder falls back to block rather than exposing that broker's host identity.
func TestHubDefaultGCPIdentity_PassthroughNotAppliedOnNonEmbeddedBroker(t *testing.T) {
	f := bypassAgentsSetup(t)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	identity := createdAgentIdentity(t, f, "hub-passthrough-remote-agent")
	assert.Equal(t, store.GCPMetadataModeBlock, identity.MetadataMode,
		"hub-default passthrough must not apply on a non-embedded broker")
}

// TestHubDefaultGCPIdentity_PassthroughNotAppliedOnSpoofedEmbeddedLabel pins
// the round-2 review: the scion.io/broker-role label is writable by the
// broker's owner, so a user-registered broker that labels itself "embedded"
// must not receive the hub-default passthrough. Only the broker the server
// itself recorded as embedded qualifies.
func TestHubDefaultGCPIdentity_PassthroughNotAppliedOnSpoofedEmbeddedLabel(t *testing.T) {
	f := bypassAgentsSetup(t)
	ctx := context.Background()
	b, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
	require.NoError(t, err)
	if b.Labels == nil {
		b.Labels = map[string]string{}
	}
	b.Labels["scion.io/broker-role"] = "embedded"
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, b))
	// The hub does have an embedded broker, just not this one.
	f.srv.SetEmbeddedBrokerID("some-other-embedded-broker")
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	identity := createdAgentIdentity(t, f, "hub-passthrough-spoofed-agent")
	assert.Equal(t, store.GCPMetadataModeBlock, identity.MetadataMode,
		"a broker-owner-set embedded label must not unlock hub-default passthrough")
}

// TestHubDefaultGCPIdentity_PassthroughSurvivesSettingsReload covers the
// round-2 boot-path blocker end to end on the hub side: the hub default is
// extracted from bootstrap koanf into the agent_defaults hub_settings row (as
// syncHubSettings does on every boot), loaded by OperationalSettings.Refresh,
// applied with ApplySnapshot, and then agent creation must still resolve
// passthrough. The cmd-side boot sequence is pinned by
// TestInitOperationalSettings_HubDefaultGCPIdentitySurvivesRestart.
func TestHubDefaultGCPIdentity_PassthroughSurvivesSettingsReload(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)

	bootK := koanf.New(".")
	require.NoError(t, bootK.Load(confmap.Provider(map[string]interface{}{
		"default_gcp_identity_mode": store.GCPMetadataModePassthrough,
	}, "."), nil))
	doc, err := opsettings.ExtractSectionFromKoanf(bootK, "agent_defaults")
	require.NoError(t, err)

	settings := newFakeHubSettingStore()
	settings.seed("agent_defaults", doc)
	ops := NewOperationalSettings(settings, bootK, emptyKoanf())
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)
	ApplySnapshot(f.srv, ops.Snapshot())

	identity := createdAgentIdentity(t, f, "hub-passthrough-reloaded-agent")
	assert.Equal(t, store.GCPMetadataModePassthrough, identity.MetadataMode)
}

// TestHubDefaultGCPIdentity_ProjectPassthroughUnaffectedByBrokerRole confirms
// the embedded-broker restriction is specific to the hub rung: a project's own
// passthrough default keeps its existing behaviour on any broker.
func TestHubDefaultGCPIdentity_ProjectPassthroughUnaffectedByBrokerRole(t *testing.T) {
	f := bypassAgentsSetup(t)
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModePassthrough
	require.NoError(t, f.store.UpdateProject(ctx, proj))

	identity := createdAgentIdentity(t, f, "project-passthrough-remote-agent")
	assert.Equal(t, store.GCPMetadataModePassthrough, identity.MetadataMode)
}

// TestHubDefaultGCPIdentity_AssignProjectScopedSAFromOtherProjectFails covers
// the per-project reachability check at dispatch. The PUT validator now
// rejects project-scoped accounts outright, but a value written before that
// check (or directly into settings.yaml) can still name one; it must fail
// creation in other projects with a clear error rather than assign it.
func TestHubDefaultGCPIdentity_AssignProjectScopedSAFromOtherProjectFails(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.other.ID, true)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: sa.ID,
	})

	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "hub-default-cross-project-agent"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "hub default GCP service account is not available in this project")
}

// TestHubDefaultGCPIdentity_ProjectExplicitBlockWinsOverHubPassthrough pins
// the distinction this change had to introduce: a project that explicitly
// opts into "block" must stop the ladder there, not fall through to a hub
// default one rung down. Before this change, "block" and "no project
// setting" were the same branch.
func TestHubDefaultGCPIdentity_ProjectExplicitBlockWinsOverHubPassthrough(t *testing.T) {
	f := bypassAgentsSetup(t)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeBlock
	require.NoError(t, f.store.UpdateProject(ctx, proj))

	identity := createdAgentIdentity(t, f, "project-explicit-block-agent")
	assert.Equal(t, store.GCPMetadataModeBlock, identity.MetadataMode,
		"an explicit project-level \"block\" must not fall through to the hub default")
}

// TestHubDefaultGCPIdentity_ProjectDefaultTakesPrecedenceOverHubDefault
// confirms the ladder order: a project default answers the question before
// the hub default is ever consulted, regardless of what the hub default
// would have produced.
func TestHubDefaultGCPIdentity_ProjectDefaultTakesPrecedenceOverHubDefault(t *testing.T) {
	f := bypassAgentsSetup(t)
	// A hub default that would produce a completely different, and
	// unreachable-from-nowhere, outcome if it were ever consulted.
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: "does-not-exist",
	})

	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModePassthrough
	require.NoError(t, f.store.UpdateProject(ctx, proj))

	identity := createdAgentIdentity(t, f, "project-over-hub-agent")
	assert.Equal(t, store.GCPMetadataModePassthrough, identity.MetadataMode,
		"the project default must win, and the hub default's unusable SA must never be looked up")
}

// TestHubDefaultGCPIdentity_AssignAppliesVerifiedHubScopedSA covers the
// "assign" hub default end to end: a verified hub-scoped service account is
// applied, and the assignment runs through the same authorization gate as
// project-default assignment (SurfaceHubDefault, mirroring SurfaceProjectDefault).
func TestHubDefaultGCPIdentity_AssignAppliesVerifiedHubScopedSA(t *testing.T) {
	f := bypassAgentsSetup(t)
	audit := &mockAuditLogger{}
	f.srv.SetAuditLogger(audit)

	setMode(f.srv, SAAssignCheckEnforce)
	f.srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@test.iam.gserviceaccount.com"})
	ensureHubMembership(context.Background(), f.store, f.owner.ID)
	sa := hubScopedSAForAgent(t, f, true)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: sa.ID,
	})

	identity := createdAgentIdentity(t, f, "hub-assign-agent")
	assert.Equal(t, store.GCPMetadataModeAssign, identity.MetadataMode)
	assert.Equal(t, sa.ID, identity.ServiceAccountID)
	assert.Equal(t, sa.Email, identity.ServiceAccountEmail)

	ev := onlySAEvent(t, audit)
	assert.Equal(t, SurfaceHubDefault, ev.Surface,
		"a hub-default assignment must be labelled as hub-default, not project-default")
}

// TestHubDefaultGCPIdentity_AssignDeniedByAuthorizationGate mirrors
// TestProjectDefaultGate_CreatorWithoutActAsDenied one rung down: a hub
// default naming an SA the immediate creator cannot act as must deny agent
// creation rather than silently falling back to block.
func TestHubDefaultGCPIdentity_AssignDeniedByAuthorizationGate(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: sa.ID,
	})

	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().DenyTarget(sa.Email, "no actAs grant for this caller"))

	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "hub-default-denied-agent"})
	require.Equal(t, http.StatusForbidden, rec.Code,
		"creator without actAs must be denied when the hub default applies; got: %s", rec.Body.String())
}

// TestHubDefaultGCPIdentity_AssignUnverifiedSAFailsCreation covers the same
// "a failed default is an error, not a silent degradation" rule the
// project-default rung enforces: an unverified hub-default SA must fail
// agent creation, not quietly fall back to block.
func TestHubDefaultGCPIdentity_AssignUnverifiedSAFailsCreation(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := hubScopedSAForAgent(t, f, false /* unverified */)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: sa.ID,
	})

	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "hub-default-unverified-agent"})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"an unverified hub-default SA must fail creation, not fall back to block; got: %s", rec.Body.String())
}
