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
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A project-default "passthrough" GCP identity meets the hub's native Cloud
// Run identity decision (hub.cloudrun_native_identity) only after the
// project-default rung has decided: the rung sets AppliedConfig.GCPIdentity,
// and the Cloud Run precheck, the native marker, the metadata seal and the
// broker's resolver only read it. These tests pin that a project default
// never reaches passthrough on a Cloud Run profile, with the experiment on
// or off, on create and on scheduled dispatch, and that the seal sends the
// broker only the hub's own decision.

// pdcrMarkCloudRunProfile replaces the fixture broker's profiles with one
// "default" Cloud Run profile whose report says native identity is
// supported, current and complete, with a block account (crProfile).
func pdcrMarkCloudRunProfile(t *testing.T, f *bypassAgentsFixture) {
	t.Helper()
	ctx := context.Background()
	b, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
	require.NoError(t, err)
	p := crProfile(time.Now())
	p.Name = "default"
	p.Type = cloudRunProfileType
	p.Available = true
	b.Profiles = []store.BrokerProfile{p}
	b.DefaultProfile = "default"
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, b))
}

// pdcrSetup is pdptSetup (a stored passthrough project default, the host SA
// registered) on a broker whose only profile is a native Cloud Run one, with
// hub.cloudrun_native_identity on when on is true and off otherwise.
func pdcrSetup(t *testing.T, embedded, admin, on bool) *bypassAgentsFixture {
	t.Helper()
	f := pdptSetup(t, embedded, admin)
	pdcrMarkCloudRunProfile(t, f)
	setCloudRunNativeIdentity(t, f.srv, on)
	return f
}

func pdcrBroker(t *testing.T, f *bypassAgentsFixture) *store.RuntimeBroker {
	t.Helper()
	b, err := f.store.GetRuntimeBroker(context.Background(), f.broker.ID)
	require.NoError(t, err)
	return b
}

// pdcrDispatcher is a dispatcher over the fixture store; gateOn puts
// hub.cloudrun_native_identity in its dispatch experiments.
func pdcrDispatcher(f *bypassAgentsFixture, gateOn bool) *HTTPAgentDispatcher {
	d := NewHTTPAgentDispatcherWithClient(f.store, &mockRuntimeBrokerClient{}, false, slog.Default())
	if gateOn {
		d.SetDispatchExperimentsProvider(func() []string { return []string{experiments.CloudRunNativeIdentity} })
	}
	return d
}

// pdcrAssertUnset checks the agent's applied identity is unset: the
// project default was not granted, so it is neither passthrough nor any
// other mode the broker would trust.
func pdcrAssertUnset(t *testing.T, agent *store.Agent, msg string) {
	t.Helper()
	require.NotNil(t, agent.AppliedConfig)
	assert.Nil(t, agent.AppliedConfig.GCPIdentity, msg)
}

// --- create, remote Cloud Run broker, experiment on ----------------------

func TestProjectDefaultPassthroughCloudRun_Create_ExperimentOnLeavesIdentityUnset(t *testing.T) {
	f := pdcrSetup(t, false, true, true)

	created := createdAgentRecord(t, f, CreateAgentRequest{Name: "pdcr-create-on"})
	pdcrAssertUnset(t, created, "a project default must not grant passthrough on a remote Cloud Run broker")

	b := pdcrBroker(t, f)
	assert.Nil(t, cloudRunIdentityRefusal(b, created.AppliedConfig.Profile, created.Name, created.AppliedConfig.GCPIdentity, time.Now()),
		"the unset identity runs as block on Cloud Run, which the reported block account allows")

	b.Profiles[0].CloudRunBlockAccount = false
	got := cloudRunIdentityRefusal(b, created.AppliedConfig.Profile, created.Name, created.AppliedConfig.GCPIdentity, time.Now())
	require.NotNil(t, got, "without a block account the unset identity is refused, never passed")
	assert.Equal(t, cloudRunRefusalNoBlockAccount, got.CloudRun)
	assert.Empty(t, got.Mode, "the refused dispatch carries no mode, so never passthrough")
}

// --- the same with the experiment off ------------------------------------

func TestProjectDefaultPassthroughCloudRun_Create_ExperimentOffLeavesIdentityUnset(t *testing.T) {
	f := pdcrSetup(t, false, true, false)

	created := createdAgentRecord(t, f, CreateAgentRequest{Name: "pdcr-create-off"})
	pdcrAssertUnset(t, created, "with the experiment off the project-default rung alone must keep the project default from granting passthrough")

	assert.NoError(t, pdcrDispatcher(f, false).cloudRunIdentityPrecheck(context.Background(), created),
		"with the experiment off the Cloud Run precheck steps aside")
}

func TestProjectDefaultPassthroughCloudRun_Scheduled_ExperimentOffLeavesIdentityUnset(t *testing.T) {
	f := pdcrSetup(t, false, true, false)

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "pdcr-sched-off"))
	agent := pdptAgent(t, f, "pdcr-sched-off")
	pdcrAssertUnset(t, agent, "with the experiment off a scheduled dispatch must not grant passthrough from a project default")
	assert.NoError(t, pdcrDispatcher(f, false).cloudRunIdentityPrecheck(context.Background(), agent))
}

// --- the experiment flips between create and dispatch --------------------

func TestProjectDefaultPassthroughCloudRun_ExperimentFlipsBetweenCreateAndDispatch(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name               string
		atCreate, atDispat bool
	}{
		{name: "on then off", atCreate: true, atDispat: false},
		{name: "off then on", atCreate: false, atDispat: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := pdcrSetup(t, false, true, tc.atCreate)
			created := createdAgentRecord(t, f, CreateAgentRequest{Name: "pdcr-flip"})
			pdcrAssertUnset(t, created, "the project default must not be granted at create")

			setCloudRunNativeIdentity(t, f.srv, tc.atDispat)
			d := pdcrDispatcher(f, tc.atDispat)
			require.NoError(t, d.cloudRunIdentityPrecheck(ctx, created),
				"the unset identity is block on Cloud Run with a block account, and nothing with the experiment off")
			dctx, err := d.markCloudRunNative(ctx, created)
			require.NoError(t, err)
			assert.Equal(t, tc.atDispat, cloudRunNativeFrom(dctx), "the native marker follows the experiment at dispatch")

			after, err := f.store.GetAgent(ctx, created.ID)
			require.NoError(t, err)
			pdcrAssertUnset(t, after, "a flip of the experiment must never turn the identity into passthrough")
		})
	}
}

// --- scheduled dispatch, remote Cloud Run broker, experiment on ----------

func TestProjectDefaultPassthroughCloudRun_Scheduled_ExperimentOnLeavesIdentityUnset(t *testing.T) {
	f := pdcrSetup(t, false, true, true)

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "pdcr-sched-on"))
	agent := pdptAgent(t, f, "pdcr-sched-on")
	pdcrAssertUnset(t, agent, "a scheduled dispatch must not grant passthrough from a project default on a remote Cloud Run broker")
	assert.Nil(t, cloudRunIdentityRefusal(pdcrBroker(t, f), agent.AppliedConfig.Profile, agent.Name, agent.AppliedConfig.GCPIdentity, time.Now()))
}

// --- embedded broker whose only profile is Cloud Run ---------------------

// The PUT gates who may set a passthrough default (one embedded broker and
// the setter's passthrough authority); it does not look at the runtime. The
// create rung gates where the default takes effect: only on a docker or
// podman profile of the embedded broker. On a Cloud Run-only embedded broker
// an authorized setter's PUT is accepted, an unauthorized one is refused, and
// the stored default stays inert: create leaves the identity unset and the
// native marker leaves the mode untouched.

func TestProjectDefaultPassthroughCloudRun_EmbeddedCloudRunOnly_PutAuthorizedAccepted(t *testing.T) {
	f := pdcrSetup(t, true, true, true)
	setProjectDefaultGCPMode(t, f, "")

	code, body := pdptPutMode(t, f, store.GCPMetadataModePassthrough)

	require.Equal(t, http.StatusOK, code, "body: %s", body)
	assert.Equal(t, store.GCPMetadataModePassthrough, pdptStoredMode(t, f),
		"the PUT accepts an authorized setter; the runtime is checked at use, on create")
}

func TestProjectDefaultPassthroughCloudRun_EmbeddedCloudRunOnly_PutUnauthorizedRefused(t *testing.T) {
	f := pdcrSetup(t, true, false, true)
	setProjectDefaultGCPMode(t, f, "")

	code, body := pdptPutMode(t, f, store.GCPMetadataModePassthrough)

	assert.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "requires broker ownership")
	assert.Empty(t, pdptStoredMode(t, f), "a refused PUT stores nothing")
}

func TestProjectDefaultPassthroughCloudRun_EmbeddedCloudRunOnly_CreateLeavesIdentityUnset(t *testing.T) {
	ctx := context.Background()
	f := pdcrSetup(t, true, true, true)

	created := createdAgentRecord(t, f, CreateAgentRequest{Name: "pdcr-embedded-cr"})
	pdcrAssertUnset(t, created, "the create rung allows only docker or podman, so a Cloud Run profile is never granted the project default")

	dctx, err := pdcrDispatcher(f, true).markCloudRunNative(ctx, created)
	require.NoError(t, err)
	assert.True(t, cloudRunNativeFrom(dctx), "the native marker is decided from the broker and profile")
	after, err := f.store.GetAgent(ctx, created.ID)
	require.NoError(t, err)
	assert.True(t, after.AppliedConfig.NativeCloudRun)
	pdcrAssertUnset(t, after, "the native marker never sets a mode")
}

func TestProjectDefaultPassthroughCloudRun_EmbeddedCloudRunOnly_ScheduledLeavesIdentityUnset(t *testing.T) {
	f := pdcrSetup(t, true, true, true)

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "pdcr-embedded-cr-sched"))
	pdcrAssertUnset(t, pdptAgent(t, f, "pdcr-embedded-cr-sched"),
		"the scheduled rung allows only docker or podman, so a Cloud Run profile is never granted the project default")
}

// --- the ladder and the embedded-broker registration state ---------------

// An explicit GCP identity on the create request is the first rung of the
// ladder: it wins over a project-default passthrough even where that default
// would be granted (the embedded docker broker, an authorized caller).
func TestProjectDefaultPassthroughCloudRun_ExplicitRequestWinsOverProjectDefault(t *testing.T) {
	f := pdptSetup(t, true, true)

	assertPassthroughGranted(t, createdAgentRecord(t, f, CreateAgentRequest{Name: "pdcr-explicit-control"}))

	created := createdAgentRecord(t, f, CreateAgentRequest{
		Name:        "pdcr-explicit-block",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModeBlock},
	})
	gcp := created.AppliedConfig.GCPIdentity
	require.NotNil(t, gcp, "the explicit identity must be applied")
	assert.Equal(t, store.GCPMetadataModeBlock, gcp.MetadataMode, "the explicit block must win over the project default")
	assert.False(t, gcp.RequireLocalRuntime, "no project-default grant is made")
}

// The project default is granted only on the hub's embedded broker. While
// co-located registration is pending, or after it failed, no broker is the
// embedded one, so the default stays unset; the registered row is the
// control. With no runtime broker resolved the rung denies before it looks
// at the registration state.
func TestProjectDefaultPassthroughCloudRun_EmbeddedRegistrationNotReadyLeavesIdentityUnset(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, f *bypassAgentsFixture)
		granted bool
	}{
		{name: "registered (control)", prepare: func(t *testing.T, f *bypassAgentsFixture) { markBrokerEmbedded(t, f) }, granted: true},
		{name: "registration pending", prepare: func(t *testing.T, f *bypassAgentsFixture) {
			prevTimeout := embeddedBrokerWaitTimeout
			embeddedBrokerWaitTimeout = 50 * time.Millisecond
			t.Cleanup(func() { embeddedBrokerWaitTimeout = prevTimeout })
			f.srv.ExpectEmbeddedBroker()
		}},
		{name: "registration failed", prepare: func(t *testing.T, f *bypassAgentsFixture) {
			f.srv.ExpectEmbeddedBroker()
			f.srv.EmbeddedBrokerRegistrationFailed(errors.New("pdcr registration failed"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := pdptSetup(t, false, true)
			tc.prepare(t, f)

			created := createdAgentRecord(t, f, CreateAgentRequest{Name: "pdcr-registration"})
			if tc.granted {
				assertPassthroughGranted(t, created)
				return
			}
			pdcrAssertUnset(t, created, "a project default must not be granted before the embedded broker is registered")
		})
	}

	// Equivalent: the empty-broker guard is redundant with the
	// registration-state check, which never matches an empty broker ID. Kept
	// as a pin.
	t.Run("no runtime broker", func(t *testing.T) {
		f := pdptSetup(t, true, true)
		allowed, profile := f.srv.projectDefaultPassthroughAllowed(context.Background(), nil, "", f.proj.ID, "pdcr-no-broker", "")
		assert.False(t, allowed, "no runtime broker resolved must deny the project default")
		assert.Empty(t, profile)
	})
}

// --- the metadata seal sends only the hub's decision ---------------------

func TestProjectDefaultPassthroughCloudRun_SealWritesHubLocalRuntimeOverPlanted(t *testing.T) {
	agent := &store.Agent{AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{
		MetadataMode:        store.GCPMetadataModePassthrough,
		RequireLocalRuntime: true,
	}}}
	env := map[string]string{
		"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "false",
		"SCION_METADATA_MODE":                  store.GCPMetadataModeBlock,
	}
	cls := map[string]api.EnvKind{}

	sealDispatchMetadataEnv(context.Background(), agent, env, &cls)

	assert.Equal(t, "true", env["SCION_METADATA_REQUIRE_LOCAL_RUNTIME"],
		"a confined project-default grant must reach the broker with the local-runtime flag, whatever was planted")
	assert.Equal(t, store.GCPMetadataModePassthrough, env["SCION_METADATA_MODE"])
	assert.Equal(t, "hub", env["SCION_METADATA_MODE_SOURCE"])
}

func TestProjectDefaultPassthroughCloudRun_SealWipesPlantedIdentityWithoutGrant(t *testing.T) {
	agent := &store.Agent{AppliedConfig: &store.AgentAppliedConfig{}}
	env := map[string]string{
		"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true",
		"SCION_METADATA_MODE":                  store.GCPMetadataModePassthrough,
		"SCION_METADATA_SA_EMAIL":              "planted@example-project.iam.gserviceaccount.com",
		"SCION_METADATA_PROJECT_ID":            "planted-project",
	}
	cls := map[string]api.EnvKind{}

	sealDispatchMetadataEnv(context.Background(), agent, env, &cls)

	for _, k := range []string{"SCION_METADATA_REQUIRE_LOCAL_RUNTIME", "SCION_METADATA_MODE", "SCION_METADATA_SA_EMAIL", "SCION_METADATA_PROJECT_ID"} {
		_, present := env[k]
		assert.False(t, present, "%s must not survive the seal when the hub granted no identity", k)
	}
	assert.Equal(t, "hub", env["SCION_METADATA_MODE_SOURCE"])
}

// --- a stale broker report is refused, never passed ----------------------

func TestProjectDefaultPassthroughCloudRun_StaleReportRefused(t *testing.T) {
	now := time.Now()
	p := crProfile(now)
	stale := now.Add(-2 * profileSAReportFreshFor)
	p.MappingsReportedAt = &stale
	b := crReportedBroker(p)

	for _, tc := range []struct {
		name  string
		gcpID *store.GCPIdentityConfig
	}{
		{name: "project default not granted (unset)", gcpID: nil},
		{name: "unconfined project-default passthrough", gcpID: crMode(store.GCPMetadataModePassthrough)},
		{name: "confined project-default grant", gcpID: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModePassthrough, RequireLocalRuntime: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cloudRunIdentityRefusal(b, "cr", "pdcr-stale", tc.gcpID, now)
			require.NotNil(t, got, "a stale report must refuse the dispatch")
			assert.Equal(t, cloudRunRefusalReportStale, got.CloudRun)
		})
	}
}

// --- a fresh report decides a confined grant as block --------------------

func TestProjectDefaultPassthroughCloudRun_FreshReportConfinedGrant(t *testing.T) {
	now := time.Now()
	confined := &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModePassthrough, RequireLocalRuntime: true}

	for _, tc := range []struct {
		name         string
		gcpID        *store.GCPIdentityConfig
		blockAccount bool
		wantRefusal  string // "" means not refused
		wantMode     string
	}{
		{name: "confined grant with a block account runs as block", gcpID: confined, blockAccount: true},
		{name: "confined grant without a block account is refused as block", gcpID: confined,
			wantRefusal: cloudRunRefusalNoBlockAccount, wantMode: store.GCPMetadataModeBlock},
		{name: "unconfined passthrough is refused as passthrough (control)", gcpID: crMode(store.GCPMetadataModePassthrough), blockAccount: true,
			wantRefusal: cloudRunRefusalPassthrough, wantMode: store.GCPMetadataModePassthrough},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := crProfile(now)
			p.CloudRunBlockAccount = tc.blockAccount
			got := cloudRunIdentityRefusal(crReportedBroker(p), "cr", "pdcr-fresh", tc.gcpID, now)
			if tc.wantRefusal == "" {
				assert.Nil(t, got, "a confined grant is decided as block, which the reported block account allows")
				return
			}
			require.NotNil(t, got, "the dispatch must be refused")
			assert.Equal(t, tc.wantRefusal, got.CloudRun)
			assert.Equal(t, tc.wantMode, got.Mode)
		})
	}
}
