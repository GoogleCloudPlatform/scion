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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

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

// markBrokerRuntimeProfile records a single runtime profile of the given
// type on the fixture's broker, standing in for a broker whose settings
// define no profiles at all (buildStoreBrokerProfiles's single-"default"
// fallback, cmd/server_broker.go). Tests use this for the simple
// one-profile case; markBrokerStockProfiles below covers the realistic
// multi-profile embedded broker.
func markBrokerRuntimeProfile(t *testing.T, f *bypassAgentsFixture, profileType string) {
	t.Helper()
	ctx := context.Background()
	b, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
	require.NoError(t, err)
	b.Profiles = []store.BrokerProfile{{Name: "default", Type: profileType, Available: true}}
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, b))
}

// markBrokerStockProfiles records the stock embedded-broker profile shape
// (pkg/config/embeds/default_settings.yaml): a "local" profile (docker) and
// a "remote" profile (kubernetes), plus DefaultProfile set to
// defaultProfileName — mirroring what a real single-node VM's embedded
// broker reports at registration (registerGlobalProjectAndBroker,
// cmd/server_broker.go). Every stock embedded broker looks like this: the
// hub default must keep working here with no explicit profile, not just on
// the single-profile shape markBrokerRuntimeProfile sets up.
func markBrokerStockProfiles(t *testing.T, f *bypassAgentsFixture, defaultProfileName string) {
	t.Helper()
	ctx := context.Background()
	b, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
	require.NoError(t, err)
	b.Profiles = []store.BrokerProfile{
		{Name: "local", Type: "docker", Available: true},
		{Name: "remote", Type: "kubernetes", Available: true},
	}
	b.DefaultProfile = defaultProfileName
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, b))
}

// createdAgentRecord creates an agent with the given request and returns the
// persisted record, for tests that need to inspect more than the resolved
// GCP identity — e.g. the pinned AppliedConfig.Profile.
func createdAgentRecord(t *testing.T, f *bypassAgentsFixture, req CreateAgentRequest) *store.Agent {
	t.Helper()
	rec := createAgentAsOwner(t, f, req)
	require.Equal(t, http.StatusCreated, rec.Code,
		"agent creation should succeed; got: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	got, err := f.store.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	return got
}

// TestHubDefaultGCPIdentity_PassthroughAppliedWhenNoProjectDefault covers the
// motivating case from the brief: a single-node VM admin sets "passthrough"
// as the hub-wide default, and a project with no default of its own inherits
// it on the embedded broker.
func TestHubDefaultGCPIdentity_PassthroughAppliedWhenNoProjectDefault(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	markBrokerRuntimeProfile(t, f, "docker")
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	identity := createdAgentIdentity(t, f, "hub-passthrough-agent")
	assert.Equal(t, store.GCPMetadataModePassthrough, identity.MetadataMode)
}

// TestHubDefaultGCPIdentity_PassthroughAppliedOnStockEmbeddedBroker is a
// regression test for the stock single-node VM shape: the embedded broker
// reports the real two-profile set (local=docker, remote=kubernetes, see
// pkg/config/embeds/default_settings.yaml) with DefaultProfile "local", and
// the agent names no profile at all. The hub default must still resolve to
// the broker's own default profile and grant passthrough — this is the
// headline case the runtime gate must not break. It must also pin the
// resolved profile onto AppliedConfig.Profile so the broker dispatches under
// exactly the profile that was checked.
func TestHubDefaultGCPIdentity_PassthroughAppliedOnStockEmbeddedBroker(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	markBrokerStockProfiles(t, f, "local")
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	agent := createdAgentRecord(t, f, CreateAgentRequest{Name: "hub-passthrough-stock-agent"})
	require.NotNil(t, agent.AppliedConfig)
	require.NotNil(t, agent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModePassthrough, agent.AppliedConfig.GCPIdentity.MetadataMode,
		"the stock embedded broker's own default profile (docker) must still get the hub default")
	assert.Equal(t, "local", agent.AppliedConfig.Profile,
		"the resolved profile must be pinned so dispatch cannot use a different one than the gate checked")
}

// TestHubDefaultGCPIdentity_PassthroughBlockedWhenBrokerDefaultProfileIsKubernetes
// covers the other half of the stock broker's default-profile resolution: an
// operator (or a hybrid-tier deployment) that points the broker's own
// default profile at the kubernetes-type one must still get block, with no
// profile named on the request.
func TestHubDefaultGCPIdentity_PassthroughBlockedWhenBrokerDefaultProfileIsKubernetes(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	markBrokerStockProfiles(t, f, "remote")
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	logs := captureDefaultSlog(t)

	identity := createdAgentIdentity(t, f, "hub-passthrough-stock-k8s-default-agent")
	assert.Equal(t, store.GCPMetadataModeBlock, identity.MetadataMode,
		"the hub default must fall to block when the broker's own default profile is kubernetes-type")
	assert.Len(t, logs.recordsContaining("runtime is not a local-container runtime"), 1)
}

// TestHubDefaultGCPIdentity_PassthroughFollowsProjectActiveProfileOnStockBroker
// covers R1: the project's active-profile setting must be the profile the
// gate evaluates, taking precedence over the broker's own default profile,
// on the realistic multi-profile broker shape. "local" resolves to docker
// and gets passthrough; a sibling case below sends "remote" and gets block.
func TestHubDefaultGCPIdentity_PassthroughFollowsProjectActiveProfileOnStockBroker(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	markBrokerStockProfiles(t, f, "remote") // broker default points at kubernetes
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingActiveProfile] = "local"
	require.NoError(t, f.store.UpdateProject(ctx, proj))

	agent := createdAgentRecord(t, f, CreateAgentRequest{Name: "hub-passthrough-project-local-agent"})
	require.NotNil(t, agent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModePassthrough, agent.AppliedConfig.GCPIdentity.MetadataMode,
		"the project's active profile (local/docker) must win over the broker's own default (remote/kubernetes)")
	assert.Equal(t, "local", agent.AppliedConfig.Profile)
}

// TestHubDefaultGCPIdentity_PassthroughBlockedByProjectActiveProfileOnStockBroker
// is the sibling of the test above: a project active profile of "remote"
// (kubernetes) must block, even though the broker's own default profile
// points at the local docker profile.
func TestHubDefaultGCPIdentity_PassthroughBlockedByProjectActiveProfileOnStockBroker(t *testing.T) {
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

	identity := createdAgentIdentity(t, f, "hub-passthrough-project-remote-agent")
	assert.Equal(t, store.GCPMetadataModeBlock, identity.MetadataMode,
		"the project's active profile (remote/kubernetes) must be what the gate evaluates, not the broker's own default")
}

// TestHubDefaultGCPIdentity_PassthroughFollowsExplicitRequestProfileOnStockBroker
// covers the top of the precedence order on the realistic multi-profile
// broker: an explicit request profile wins over both the project's active
// profile and the broker's own default. "remote" blocks; the sibling test
// below sends "local" and gets passthrough.
func TestHubDefaultGCPIdentity_PassthroughFollowsExplicitRequestProfileOnStockBroker(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	markBrokerStockProfiles(t, f, "local")
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	agent := createdAgentRecord(t, f, CreateAgentRequest{Name: "hub-passthrough-explicit-remote-agent", Profile: "remote"})
	require.NotNil(t, agent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeBlock, agent.AppliedConfig.GCPIdentity.MetadataMode,
		"an explicit kubernetes-type request profile must block even though the broker default is docker")
}

// TestHubDefaultGCPIdentity_PassthroughExplicitRequestProfileLocalOnStockBroker
// is the passthrough-granting sibling: an explicit request profile of
// "local" (docker) gets passthrough on the stock broker.
func TestHubDefaultGCPIdentity_PassthroughExplicitRequestProfileLocalOnStockBroker(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	markBrokerStockProfiles(t, f, "remote") // broker default points at kubernetes
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	agent := createdAgentRecord(t, f, CreateAgentRequest{Name: "hub-passthrough-explicit-local-agent", Profile: "local"})
	require.NotNil(t, agent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModePassthrough, agent.AppliedConfig.GCPIdentity.MetadataMode,
		"an explicit docker request profile must get passthrough even though the broker default is kubernetes")
	assert.Equal(t, "local", agent.AppliedConfig.Profile)
}

// TestHubDefaultGCPIdentity_PassthroughBlockedOnKubernetesRuntime pins the
// runtime-aware gate: the embedded broker qualifies, but its resolved
// runtime profile is kubernetes-type, so the hub default falls to block.
func TestHubDefaultGCPIdentity_PassthroughBlockedOnKubernetesRuntime(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	markBrokerRuntimeProfile(t, f, "kubernetes")
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	logs := captureDefaultSlog(t)

	identity := createdAgentIdentity(t, f, "hub-passthrough-k8s-agent")
	assert.Equal(t, store.GCPMetadataModeBlock, identity.MetadataMode,
		"hub-default passthrough must not apply when the resolved runtime is kubernetes-type")
	assert.Len(t, logs.recordsContaining("runtime is not a local-container runtime"), 1)
}

// TestHubDefaultGCPIdentity_PassthroughBlockedOnUnresolvableRuntime covers the
// fail-closed side of the runtime gate: the embedded broker reports no
// runtime profile and no default profile at all, so the dispatch's runtime
// cannot be resolved. An unresolvable runtime must not default to
// passthrough.
func TestHubDefaultGCPIdentity_PassthroughBlockedOnUnresolvableRuntime(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	// No profile recorded on the broker — nothing to resolve.
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	logs := captureDefaultSlog(t)

	identity := createdAgentIdentity(t, f, "hub-passthrough-unresolvable-agent")
	assert.Equal(t, store.GCPMetadataModeBlock, identity.MetadataMode,
		"hub-default passthrough must not apply when the runtime cannot be resolved")
	assert.Len(t, logs.recordsContaining("runtime profile could not be resolved"), 1)
}

// TestHubDefaultGCPIdentity_PassthroughBlockedOnAmbiguousMultiProfileBroker
// covers the ambiguous case distinct from the fully-unresolvable one above: a
// broker reports more than one profile but no DefaultProfile (e.g. a record
// written before this field existed) and the request/project name none
// either. There is no single profile to fall back to, so this must also
// block rather than guess.
func TestHubDefaultGCPIdentity_PassthroughBlockedOnAmbiguousMultiProfileBroker(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	markBrokerStockProfiles(t, f, "") // two profiles, no default recorded
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	logs := captureDefaultSlog(t)

	identity := createdAgentIdentity(t, f, "hub-passthrough-ambiguous-agent")
	assert.Equal(t, store.GCPMetadataModeBlock, identity.MetadataMode,
		"an ambiguous multi-profile broker with no resolvable default must not default to passthrough")
	assert.Len(t, logs.recordsContaining("runtime profile could not be resolved"), 1)
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

	logs := captureDefaultSlog(t)

	identity := createdAgentIdentity(t, f, "hub-passthrough-remote-agent")
	assert.Equal(t, store.GCPMetadataModeBlock, identity.MetadataMode,
		"hub-default passthrough must not apply on a non-embedded broker")
	// This hub has no embedded broker at all; the log says so rather than
	// blaming the broker.
	assert.Len(t, logs.recordsContaining("hub has no embedded broker registered"), 1)
	assert.Empty(t, logs.recordsContaining("broker is not the hub's embedded broker"))
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

	logs := captureDefaultSlog(t)

	identity := createdAgentIdentity(t, f, "hub-passthrough-spoofed-agent")
	assert.Equal(t, store.GCPMetadataModeBlock, identity.MetadataMode,
		"a broker-owner-set embedded label must not unlock hub-default passthrough")
	assert.Len(t, logs.recordsContaining("broker is not the hub's embedded broker"), 1)
}

// captureDefaultSlog routes the default slog logger into a capturing handler
// for the duration of the test.
func captureDefaultSlog(t *testing.T) *levelCapturingHandler {
	t.Helper()
	h := &levelCapturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// TestHubDefaultGCPIdentity_PassthroughWaitsForPendingEmbeddedRegistration pins
// round-3 N-1: the Hub API serves before the co-located broker registers, so
// startup marks the embedded broker as expected. A create in that window must
// wait for registration and get the hub-default passthrough, not have block
// written permanently into its applied config.
func TestHubDefaultGCPIdentity_PassthroughWaitsForPendingEmbeddedRegistration(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerRuntimeProfile(t, f, "docker")
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})
	f.srv.ExpectEmbeddedBroker()

	registered := make(chan struct{})
	go func() {
		defer close(registered)
		time.Sleep(100 * time.Millisecond)
		f.srv.SetEmbeddedBrokerID(f.broker.ID)
	}()

	identity := createdAgentIdentity(t, f, "hub-passthrough-startup-agent")
	<-registered
	assert.Equal(t, store.GCPMetadataModePassthrough, identity.MetadataMode)
}

// TestHubDefaultGCPIdentity_PendingRegistrationTimeoutFallsBackToBlock covers
// the bound on that wait: if registration never resolves, the create falls
// back to block and the log names the pending registration as the cause.
func TestHubDefaultGCPIdentity_PendingRegistrationTimeoutFallsBackToBlock(t *testing.T) {
	f := bypassAgentsSetup(t)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})
	prevTimeout := embeddedBrokerWaitTimeout
	embeddedBrokerWaitTimeout = 50 * time.Millisecond
	t.Cleanup(func() { embeddedBrokerWaitTimeout = prevTimeout })
	f.srv.ExpectEmbeddedBroker()
	logs := captureDefaultSlog(t)

	identity := createdAgentIdentity(t, f, "hub-passthrough-pending-agent")
	assert.Equal(t, store.GCPMetadataModeBlock, identity.MetadataMode)
	assert.Len(t, logs.recordsContaining("co-located broker registration still pending"), 1)
}

// TestHubDefaultGCPIdentity_RegistrationFailedLogsDistinctCause pins round-3
// N-2: when co-located registration failed at startup the hub has no embedded
// broker, and the log must say that rather than "not the hub's embedded
// broker", which would send an operator looking at the wrong thing.
func TestHubDefaultGCPIdentity_RegistrationFailedLogsDistinctCause(t *testing.T) {
	f := bypassAgentsSetup(t)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})
	f.srv.ExpectEmbeddedBroker()
	f.srv.EmbeddedBrokerRegistrationFailed(errors.New("database unavailable"))
	logs := captureDefaultSlog(t)

	start := time.Now()
	identity := createdAgentIdentity(t, f, "hub-passthrough-regfail-agent")
	assert.Equal(t, store.GCPMetadataModeBlock, identity.MetadataMode)
	assert.Less(t, time.Since(start), embeddedBrokerWaitTimeout,
		"a failed registration must release waiters, not run out the wait")

	recs := logs.recordsContaining("co-located broker registration failed at startup")
	require.Len(t, recs, 1)
	var regErr string
	recs[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "registration_error" {
			regErr = a.Value.String()
		}
		return true
	})
	assert.Equal(t, "database unavailable", regErr)
	assert.Empty(t, logs.recordsContaining("broker is not the hub's embedded broker"))
	assert.Empty(t, logs.recordsContaining("hub has no embedded broker registered"))
}

// TestExpectEmbeddedBroker_StatelessRegistrationReleasesWaiters covers the
// Cloud Run path: startup registers the co-located broker through
// SetStatelessEmbeddedBrokerID, which must release a pending wait just as
// SetEmbeddedBrokerID does. The wait timeout is raised well above the test's
// deadline so that a missing release shows up as a failure, not a slow pass.
func TestExpectEmbeddedBroker_StatelessRegistrationReleasesWaiters(t *testing.T) {
	prevTimeout := embeddedBrokerWaitTimeout
	embeddedBrokerWaitTimeout = time.Hour
	t.Cleanup(func() { embeddedBrokerWaitTimeout = prevTimeout })

	srv := &Server{}
	srv.ExpectEmbeddedBroker()

	done := make(chan embeddedBrokerState, 1)
	go func() { done <- srv.waitForEmbeddedBroker(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	srv.SetStatelessEmbeddedBrokerID("cloudrun-broker")

	select {
	case state := <-done:
		assert.Equal(t, embeddedBrokerState{id: "cloudrun-broker"}, state)
	case <-time.After(5 * time.Second):
		t.Fatal("SetStatelessEmbeddedBrokerID did not release the pending embedded-broker wait")
	}
	assert.True(t, srv.isEmbeddedBroker("cloudrun-broker"))
	assert.Equal(t, "cloudrun-broker", srv.GetStatelessEmbeddedBrokerID())
}

// TestExpectEmbeddedBroker_Lifecycle checks the expectation's edge cases:
// it is a no-op once an embedded broker is known, and resolving it more than
// once (a later SetEmbeddedBrokerID) does not panic on a closed channel.
func TestExpectEmbeddedBroker_Lifecycle(t *testing.T) {
	srv := &Server{}
	srv.SetEmbeddedBrokerID("b1")
	srv.ExpectEmbeddedBroker()
	assert.Nil(t, srv.embeddedBrokerPending, "no pending wait once the embedded broker is known")

	srv2 := &Server{}
	srv2.ExpectEmbeddedBroker()
	require.NotNil(t, srv2.embeddedBrokerPending)
	srv2.SetEmbeddedBrokerID("b2")
	srv2.SetEmbeddedBrokerID("b2")
	state := srv2.waitForEmbeddedBroker(context.Background())
	assert.Equal(t, embeddedBrokerState{id: "b2"}, state)
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
	markBrokerRuntimeProfile(t, f, "docker")

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

// TestHubDefaultGCPIdentity_ProjectPassthroughUnaffectedByKubernetesRuntime
// confirms the runtime gate is specific to the hub-default rung: a project's
// own passthrough default is a deliberate operator choice, not a default,
// and keeps applying regardless of the resolved runtime — including on a
// kubernetes-type profile, where the hub default falls to block.
func TestHubDefaultGCPIdentity_ProjectPassthroughUnaffectedByKubernetesRuntime(t *testing.T) {
	f := bypassAgentsSetup(t)
	ctx := context.Background()
	markBrokerEmbedded(t, f)
	markBrokerRuntimeProfile(t, f, "kubernetes")
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModePassthrough
	require.NoError(t, f.store.UpdateProject(ctx, proj))

	identity := createdAgentIdentity(t, f, "project-passthrough-k8s-agent")
	assert.Equal(t, store.GCPMetadataModePassthrough, identity.MetadataMode,
		"a project-level passthrough default must not be gated by the hub-default runtime check")
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

// TestResolveAgentRuntimeProfileType covers the pure profile-resolution
// helper directly, including cases the higher-level hub-default tests above
// don't exercise on their own: the broker's own DefaultProfile fallback when
// profileName is empty, a named profile that doesn't exist on the broker, and
// a broker with more than one profile and neither an explicit selection nor a
// resolvable default. All of the unresolvable cases must report ok=false
// rather than guessing.
func TestResolveAgentRuntimeProfileType(t *testing.T) {
	docker := store.BrokerProfile{Name: "default", Type: "docker", Available: true}
	local := store.BrokerProfile{Name: "local", Type: "docker", Available: true}
	remote := store.BrokerProfile{Name: "remote", Type: "kubernetes", Available: true}
	k8s := store.BrokerProfile{Name: "gke", Type: "kubernetes", Available: true}

	tests := []struct {
		name           string
		profiles       []store.BrokerProfile
		defaultProfile string
		profileName    string
		wantName       string
		wantType       string
		wantOK         bool
	}{
		{"single profile, no explicit selection, no default recorded", []store.BrokerProfile{docker}, "", "", "default", "docker", true},
		{"single profile, matching name", []store.BrokerProfile{docker}, "", "default", "default", "docker", true},
		{"named profile found among several", []store.BrokerProfile{docker, k8s}, "", "gke", "gke", "kubernetes", true},
		{"named profile not found", []store.BrokerProfile{docker}, "", "gke", "", "", false},
		{"stock broker, empty selection resolves via DefaultProfile=local", []store.BrokerProfile{local, remote}, "local", "", "local", "docker", true},
		{"stock broker, empty selection resolves via DefaultProfile=remote", []store.BrokerProfile{local, remote}, "remote", "", "remote", "kubernetes", true},
		{"stock broker, explicit selection wins over DefaultProfile", []store.BrokerProfile{local, remote}, "local", "remote", "remote", "kubernetes", true},
		{"stock broker, DefaultProfile not one of the broker's profiles", []store.BrokerProfile{local, remote}, "gke", "", "", "", false},
		{"multiple profiles, no explicit selection, no default recorded", []store.BrokerProfile{docker, k8s}, "", "", "", "", false},
		{"no profiles at all", nil, "", "", "", "", false},
		{"no profiles at all, named selection", nil, "", "default", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			broker := &store.RuntimeBroker{Profiles: tc.profiles, DefaultProfile: tc.defaultProfile}
			gotName, gotType, gotOK := resolveAgentRuntimeProfileType(broker, tc.profileName)
			assert.Equal(t, tc.wantOK, gotOK)
			assert.Equal(t, tc.wantName, gotName)
			assert.Equal(t, tc.wantType, gotType)
		})
	}
}

// TestEffectiveRuntimeProfileName covers the precedence helper directly:
// request profile, else the project's active-profile annotation, with no
// further fallback (the broker's own default is resolved separately, inside
// resolveAgentRuntimeProfileType, once a broker record is available).
func TestEffectiveRuntimeProfileName(t *testing.T) {
	projectWithActive := &store.Project{Annotations: map[string]string{projectSettingActiveProfile: "local"}}
	projectWithoutActive := &store.Project{Annotations: map[string]string{}}

	tests := []struct {
		name           string
		requestProfile string
		project        *store.Project
		want           string
	}{
		{"request profile wins over project", "remote", projectWithActive, "remote"},
		{"falls back to project active profile", "", projectWithActive, "local"},
		{"empty when neither is set", "", projectWithoutActive, ""},
		{"empty when project is nil", "", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveRuntimeProfileName(tc.requestProfile, tc.project)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestHubDefaultGCPIdentity_PinnedProfileSurvivesReincarnateAfterProjectActiveProfileChanges
// confirms the pin is replayed by scion reincarnate (buildFreshAppliedConfig
// reads AppliedConfig.CreateInputs, not the live AppliedConfig), so the
// pinned profile is what reincarnate uses even after the project's active
// profile has since changed.
func TestHubDefaultGCPIdentity_PinnedProfileSurvivesReincarnateAfterProjectActiveProfileChanges(t *testing.T) {
	f := bypassAgentsSetup(t)
	markBrokerEmbedded(t, f)
	markBrokerStockProfiles(t, f, "local")
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
	})

	agent := createdAgentRecord(t, f, CreateAgentRequest{Name: "hub-passthrough-reincarnate-agent"})
	require.NotNil(t, agent.AppliedConfig)
	require.NotNil(t, agent.AppliedConfig.GCPIdentity)
	require.Equal(t, store.GCPMetadataModePassthrough, agent.AppliedConfig.GCPIdentity.MetadataMode)
	require.Equal(t, "local", agent.AppliedConfig.Profile)
	require.NotNil(t, agent.AppliedConfig.CreateInputs)
	require.Equal(t, "local", agent.AppliedConfig.CreateInputs.Profile,
		"the pin must also reach CreateInputs.Profile, or reincarnate loses it")

	// An operator (or the project owner) changes the project's active
	// profile after the grant — the pin must survive this.
	ctx := context.Background()
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

// TestHubDefaultGCPIdentity_RequireLocalRuntimeSetOnlyOnHubDefaultGrant
// covers the broker-side re-check's own scope boundary from the hub side:
// RequireLocalRuntime — the flag that tells the broker to re-verify a
// passthrough grant against the runtime it actually resolves — must be true
// on a hub-default grant and false (unset) on every other passthrough
// source. The hub resolves an agent's runtime from the broker's
// registration-time data, which can disagree with the broker's own
// dispatch-time settings; project-level and explicit passthrough are
// deliberate operator/caller choices, not defaults, and must not pay that
// re-check.
func TestHubDefaultGCPIdentity_RequireLocalRuntimeSetOnlyOnHubDefaultGrant(t *testing.T) {
	t.Run("hub-default grant sets the flag", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		markBrokerEmbedded(t, f)
		markBrokerRuntimeProfile(t, f, "docker")
		setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
			DefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
		})

		identity := createdAgentIdentity(t, f, "hub-default-flag-agent")
		assert.Equal(t, store.GCPMetadataModePassthrough, identity.MetadataMode)
		assert.True(t, identity.RequireLocalRuntime,
			"a hub-default passthrough grant must set RequireLocalRuntime")
	})

	t.Run("project-level passthrough does not set the flag", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		ctx := context.Background()
		proj, err := f.store.GetProject(ctx, f.proj.ID)
		require.NoError(t, err)
		if proj.Annotations == nil {
			proj.Annotations = map[string]string{}
		}
		proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModePassthrough
		require.NoError(t, f.store.UpdateProject(ctx, proj))

		identity := createdAgentIdentity(t, f, "project-passthrough-flag-agent")
		assert.Equal(t, store.GCPMetadataModePassthrough, identity.MetadataMode)
		assert.False(t, identity.RequireLocalRuntime,
			"a project-level passthrough default must never set RequireLocalRuntime")
	})

	// Explicit request passthrough is covered by TestPassthrough_* in
	// passthrough_gate_test.go, which exercises the broker-owner/actAs
	// authorization path this fixture does not set up. Those construct
	// store.GCPIdentityConfig without RequireLocalRuntime (it defaults to
	// false), and a repo-wide check confirms only the two hub-default call
	// sites (handlers_agents_core.go, server.go) ever set it to true.
}
