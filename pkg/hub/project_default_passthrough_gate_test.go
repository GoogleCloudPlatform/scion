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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A project-default "passthrough" GCP identity is held to the passthrough
// checks on every surface: the hub-default confinement (embedded broker,
// local container runtime) and the caller's passthrough authority (broker
// owner or admin, host SA registered, actAs) when it is applied on agent
// create and on scheduled dispatch, and the setter's passthrough authority
// when it is saved. The consume-side tests seed the default straight into the
// store, so they also cover a default saved before the save-side check.

const pdptHostSA = "broker-host@pdpt-project.iam.gserviceaccount.com"

// pdptSetup returns the shared fixture with a project-default passthrough
// stored, and the broker's host service account registered. embedded marks
// the broker as the hub's embedded broker; admin makes the fixture owner a
// hub admin, which satisfies the passthrough ownership check (the fixture
// broker has no recorded owner). Caller-permission checking stays off, so
// actAs is not what decides these tests.
func pdptSetup(t *testing.T, embedded, admin bool) *bypassAgentsFixture {
	t.Helper()
	f := bypassAgentsSetup(t)
	if embedded {
		markBrokerEmbedded(t, f)
	}
	markBrokerRuntimeProfile(t, f, "docker")
	_, err := f.store.SetRuntimeBrokerGCPHost(context.Background(), f.broker.ID, pdptHostSA, "pdpt-project")
	require.NoError(t, err)
	if admin {
		grantSuperAdmin(t, f.store, f.owner.ID)
	}
	setProjectDefaultGCPMode(t, f, store.GCPMetadataModePassthrough)
	return f
}

func pdptAgent(t *testing.T, f *bypassAgentsFixture, name string) *store.Agent {
	t.Helper()
	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, name)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	return got
}

// assertPassthroughGranted checks a granted project default carries the
// hub-default grant's shape: passthrough, flagged for the broker's runtime
// re-check, with the checked profile pinned for dispatch and reincarnate.
func assertPassthroughGranted(t *testing.T, agent *store.Agent) {
	t.Helper()
	gcp := agent.AppliedConfig.GCPIdentity
	require.NotNil(t, gcp)
	assert.Equal(t, store.GCPMetadataModePassthrough, gcp.MetadataMode)
	assert.True(t, gcp.RequireLocalRuntime, "a project-default grant must ask the broker to re-check the runtime")
	assert.Equal(t, "default", agent.AppliedConfig.Profile, "the checked profile must be pinned")
	require.NotNil(t, agent.AppliedConfig.CreateInputs)
	assert.Equal(t, "default", agent.AppliedConfig.CreateInputs.Profile, "the pin must survive reincarnate")
}

// --- agent create ---------------------------------------------------------

func TestProjectDefaultPassthrough_Create_GrantedForAuthorizedCallerOnEmbeddedBroker(t *testing.T) {
	f := pdptSetup(t, true, true)

	assertPassthroughGranted(t, createdAgentRecord(t, f, CreateAgentRequest{Name: "pdpt-create-grant"}))
}

// The caller is not the broker owner or an admin: the authority check
// refuses, and the default is left unset rather than granted.
func TestProjectDefaultPassthrough_Create_CallerWithoutAuthorityLeavesIdentityUnset(t *testing.T) {
	f := pdptSetup(t, true, false)

	assert.Nil(t, createdAgentIdentityOrNil(t, f, "pdpt-create-noauth"),
		"a caller without passthrough authority must not get the broker identity from a project default")
}

// The caller has the authority, but the broker is not the embedded one: the
// confinement refuses, as for the hub default.
func TestProjectDefaultPassthrough_Create_NonEmbeddedBrokerLeavesIdentityUnset(t *testing.T) {
	f := pdptSetup(t, false, true)

	assert.Nil(t, createdAgentIdentityOrNil(t, f, "pdpt-create-remote"),
		"a project default must not grant passthrough outside the embedded broker")
}

// --- scheduled dispatch ---------------------------------------------------

func TestProjectDefaultPassthrough_Scheduled_GrantedForAuthorizedPrincipalOnEmbeddedBroker(t *testing.T) {
	f := pdptSetup(t, true, true)

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "pdpt-sched-grant"))
	assertPassthroughGranted(t, pdptAgent(t, f, "pdpt-sched-grant"))
}

// The schedule's principal is not the broker owner or an admin: the
// authority check refuses, and the default is left unset.
func TestProjectDefaultPassthrough_Scheduled_PrincipalWithoutAuthorityLeavesIdentityUnset(t *testing.T) {
	f := pdptSetup(t, true, false)

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "pdpt-sched-noauth"))
	assert.Nil(t, pdptAgent(t, f, "pdpt-sched-noauth").AppliedConfig.GCPIdentity,
		"a schedule principal without passthrough authority must not get the broker identity from a project default")
}

func TestProjectDefaultPassthrough_Scheduled_NonEmbeddedBrokerLeavesIdentityUnset(t *testing.T) {
	f := pdptSetup(t, false, true)

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "pdpt-sched-remote"))
	assert.Nil(t, pdptAgent(t, f, "pdpt-sched-remote").AppliedConfig.GCPIdentity,
		"a project default must not grant passthrough outside the embedded broker")
}

// --- project settings PUT -------------------------------------------------

// pdptPutMode PUTs the project's default GCP identity mode as the fixture
// owner (bound as project owner) and returns the response code and body.
func pdptPutMode(t *testing.T, f *bypassAgentsFixture, mode string) (int, string) {
	t.Helper()
	ctx := context.Background()
	f.srv.seedProjectCreatorMembership(ctx, f.proj)
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, f.proj.ID, f.owner.ID))
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPut,
		"/api/v1/projects/"+f.proj.ID+"/settings",
		map[string]interface{}{"defaultGCPIdentityMode": mode})
	return rec.Code, rec.Body.String()
}

func pdptStoredMode(t *testing.T, f *bypassAgentsFixture) string {
	t.Helper()
	proj, err := f.store.GetProject(context.Background(), f.proj.ID)
	require.NoError(t, err)
	return proj.Annotations[projectSettingDefaultGCPIdentityMode]
}

// pdptPutSetup is pdptSetup with no default stored yet.
func pdptPutSetup(t *testing.T, embedded, admin bool) *bypassAgentsFixture {
	t.Helper()
	f := pdptSetup(t, embedded, admin)
	setProjectDefaultGCPMode(t, f, "")
	return f
}

// A project owner who is not the broker owner or an admin cannot save a
// passthrough default.
func TestProjectDefaultPassthrough_Put_SetterWithoutAuthorityRefused(t *testing.T) {
	f := pdptPutSetup(t, true, false)

	code, body := pdptPutMode(t, f, store.GCPMetadataModePassthrough)

	assert.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "requires broker ownership")
	assert.Empty(t, pdptStoredMode(t, f), "a refused PUT stores nothing")
}

func TestProjectDefaultPassthrough_Put_AuthorizedSetterAccepted(t *testing.T) {
	f := pdptPutSetup(t, true, true)

	code, body := pdptPutMode(t, f, store.GCPMetadataModePassthrough)

	require.Equal(t, http.StatusOK, code, "body: %s", body)
	assert.Equal(t, store.GCPMetadataModePassthrough, pdptStoredMode(t, f))
}

// With no embedded broker, a project default can never grant passthrough,
// so saving one is refused even for an admin.
func TestProjectDefaultPassthrough_Put_NoEmbeddedBrokerRefused(t *testing.T) {
	f := pdptPutSetup(t, false, true)

	code, body := pdptPutMode(t, f, store.GCPMetadataModePassthrough)

	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "embedded broker")
	assert.Empty(t, pdptStoredMode(t, f))
}

// Moving a stored passthrough default away from passthrough is always
// allowed: it is the escape from a default the setter could not save.
func TestProjectDefaultPassthrough_Put_MovingAwayFromPassthroughAlwaysAllowed(t *testing.T) {
	f := pdptSetup(t, true, false)

	code, body := pdptPutMode(t, f, store.GCPMetadataModeBlock)

	require.Equal(t, http.StatusOK, code, "body: %s", body)
	assert.Equal(t, store.GCPMetadataModeBlock, pdptStoredMode(t, f))
}
