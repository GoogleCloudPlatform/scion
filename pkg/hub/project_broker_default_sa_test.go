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
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Per-broker default GCP service account (ptone/scion#8184 / ptone/scion#4895):
// a project can set a default registered service account per flat Runtime Broker
// (defaultGCPIdentityServiceAccountIDByBroker). On agent create and scheduled
// dispatch, when no explicit GCP identity is requested and the resolved target
// Runtime Broker is flat, that broker's entry wins over the project and hub
// defaults.

func createSettingsFlatBroker(t *testing.T, s store.Store, idSuffix, name, targetType string) *store.RuntimeBroker {
	t.Helper()
	b := &store.RuntimeBroker{
		ID:           tid("settings-flat-" + idSuffix + "-" + t.Name()),
		Name:         name,
		Slug:         name,
		Status:       store.BrokerStatusOnline,
		Endpoint:     "http://" + name + ".invalid",
		Capabilities: &store.BrokerCapabilities{Reprovision: true, Sync: true},
		RuntimeTarget: &api.RuntimeTargetDescriptor{
			ID:          tid("settings-target-" + idSuffix + "-" + t.Name()),
			Type:        targetType,
			DisplayName: name,
		},
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), b))
	return b
}

func TestProjectSettings_BrokerDefaultSA_RoundTripPreserveClearAndResolved(t *testing.T) {
	srv, s := testServer(t)
	setFlatExperiment(t, srv, true)
	project := createTestProjectForSettings(t, s)
	flat := createSettingsFlatBroker(t, s, "cr", "cr-east", "cloudrun")
	sa := newSettingsTestSA(t, s, project.ID, "sa-broker")
	url := "/api/v1/projects/" + project.ID + "/settings"

	rec := doRequest(t, srv, http.MethodPut, url, hubclient.ProjectSettings{
		DefaultGCPIdentityServiceAccountIDByBroker: map[string]string{flat.ID: sa.ID},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, map[string]string{flat.ID: sa.ID},
		getProjectSettings(t, srv, project.ID).DefaultGCPIdentityServiceAccountIDByBroker)

	// Resolved settings endpoint reports the annotation key with hubSourceNone ("absent").
	resRec := doRequest(t, srv, http.MethodGet, url+"/resolved", nil)
	require.Equal(t, http.StatusOK, resRec.Code, "body: %s", resRec.Body.String())
	var resolved hubclient.ResolvedProjectSettings
	require.NoError(t, json.NewDecoder(resRec.Body).Decode(&resolved))
	require.NotNil(t, resolved.Project)
	assert.Equal(t, map[string]string{flat.ID: sa.ID}, resolved.Project.DefaultGCPIdentityServiceAccountIDByBroker)
	entry, ok := resolved.Settings[projectSettingDefaultGCPIdentitySAIDByBroker]
	require.True(t, ok, "resolved settings must include %s", projectSettingDefaultGCPIdentitySAIDByBroker)
	assert.True(t, entry.ProjectSet)
	assert.Equal(t, hubclient.ResolvedHubDefaultAbsent, entry.HubDefault)

	// Partial PUT omitting the field preserves the stored map.
	rec = doRequest(t, srv, http.MethodPut, url, map[string]interface{}{"defaultModel": "m"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, map[string]string{flat.ID: sa.ID},
		getProjectSettings(t, srv, project.ID).DefaultGCPIdentityServiceAccountIDByBroker)

	// Empty object clears the map and deletes the underlying annotation.
	rec = doRequest(t, srv, http.MethodPut, url, map[string]interface{}{
		"defaultGCPIdentityServiceAccountIDByBroker": map[string]string{},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Nil(t, getProjectSettings(t, srv, project.ID).DefaultGCPIdentityServiceAccountIDByBroker)
	stored, err := s.GetProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.NotContains(t, stored.Annotations, projectSettingDefaultGCPIdentitySAIDByBroker)
}

func TestProjectSettings_BrokerDefaultSA_ExperimentGate(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: false, linkFlat: true})
	sa := newSettingsTestSA(t, f.s, f.project.ID, "sa-exp")
	url := "/api/v1/projects/" + f.project.ID + "/settings"

	// A no-op full-body PUT sending defaultGCPIdentityServiceAccountIDByBroker: {} when nothing
	// is stored (stored == nil) succeeds cleanly while hub.flat_runtime_brokers is off.
	rec := doRequest(t, f.srv, http.MethodPut, url, map[string]interface{}{
		"defaultModel": "claude-haiku",
		"defaultGCPIdentityServiceAccountIDByBroker": map[string]string{},
	})
	require.Equal(t, http.StatusOK, rec.Code, "empty-map no-op PUT with nothing stored must succeed when experiment is off: %s", rec.Body.String())
	assert.Nil(t, getProjectSettings(t, f.srv, f.project.ID).DefaultGCPIdentityServiceAccountIDByBroker)

	// Writing a new per-broker default while hub.flat_runtime_brokers is off returns 412 experiment_disabled.
	rec = doRequest(t, f.srv, http.MethodPut, url, map[string]interface{}{
		"defaultGCPIdentityServiceAccountIDByBroker": map[string]string{f.flat.ID: sa.ID},
	})
	details := requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
	assert.Equal(t, experiments.FlatRuntimeBrokers, details["experiment"])
	assert.Nil(t, getProjectSettings(t, f.srv, f.project.ID).DefaultGCPIdentityServiceAccountIDByBroker)

	// Enable experiment, store an entry, then disable experiment again:
	// - GET still succeeds cleanly
	// - Unrelated partial PUT and full-body PUT carrying the unchanged map succeed (no wedge)
	// - Modifying the map while experiment is off is refused with 412 experiment_disabled
	// - Read helper ignores the stored annotation while experiment is off
	setFlatExperiment(t, f.srv, true)
	rec = doRequest(t, f.srv, http.MethodPut, url, map[string]interface{}{
		"defaultGCPIdentityServiceAccountIDByBroker": map[string]string{f.flat.ID: sa.ID},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	setFlatExperiment(t, f.srv, false)
	assert.Equal(t, map[string]string{f.flat.ID: sa.ID},
		getProjectSettings(t, f.srv, f.project.ID).DefaultGCPIdentityServiceAccountIDByBroker)

	rec = doRequest(t, f.srv, http.MethodPut, url, map[string]interface{}{"defaultModel": "claude-sonnet"})
	require.Equal(t, http.StatusOK, rec.Code, "unrelated partial PUT must succeed when experiment is off: %s", rec.Body.String())

	rec = doRequest(t, f.srv, http.MethodPut, url, map[string]interface{}{
		"defaultModel": "claude-opus",
		"defaultGCPIdentityServiceAccountIDByBroker": map[string]string{f.flat.ID: sa.ID},
	})
	require.Equal(t, http.StatusOK, rec.Code, "full-body PUT keeping unchanged map must not wedge unrelated edits: %s", rec.Body.String())

	otherSA := newSettingsTestSA(t, f.s, f.project.ID, "sa-exp-2")
	rec = doRequest(t, f.srv, http.MethodPut, url, map[string]interface{}{
		"defaultGCPIdentityServiceAccountIDByBroker": map[string]string{f.flat.ID: otherSA.ID},
	})
	details = requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
	assert.Equal(t, experiments.FlatRuntimeBrokers, details["experiment"])

	projReloaded, err := f.s.GetProject(context.Background(), f.project.ID)
	require.NoError(t, err)
	pinned := &store.Agent{
		ID: "a1", ProjectID: f.project.ID, RuntimeBrokerID: f.flat.ID,
		PinnedRuntimeBrokerID: f.flat.ID, PinnedRuntimeTargetID: f.flat.RuntimeTarget.ID, PinnedRuntimeTargetType: f.flat.RuntimeTarget.Type,
	}
	gotSA, err := f.srv.brokerDefaultSA(context.Background(), pinned, f.flat.ID, projReloaded)
	require.NoError(t, err)
	assert.Empty(t, gotSA, "by-broker default must be ignored at read when hub.flat_runtime_brokers is off")
}

func TestProjectSettings_BrokerDefaultSA_ValidationAndAuthz(t *testing.T) {
	var failSAID string
	srv, s := testServer(t)
	_, fault := installStoreFault(t, srv, func(inner store.Store, fault *storeFaultSwitch) store.Store {
		return &failedBrokerSAStatusStore{Store: inner, fault: fault, saID: &failSAID}
	})
	setFlatExperiment(t, srv, true)

	project := createTestProjectForSettings(t, s)
	flatDocker := createSettingsFlatBroker(t, s, "docker", "flat-docker", "docker")
	flatK8s := createSettingsFlatBroker(t, s, "k8s", "flat-k8s", "kubernetes")
	flatCR := createSettingsFlatBroker(t, s, "cr", "flat-cr", "cloudrun")
	legacy := &store.RuntimeBroker{
		ID:             tid("settings-legacy-" + t.Name()),
		Name:           "legacy-profile-broker",
		Slug:           "legacy-profile-broker",
		Status:         store.BrokerStatusOnline,
		Profiles:       []store.BrokerProfile{{Name: "local", Type: "docker", Available: true}},
		DefaultProfile: "local",
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), legacy))

	good := newSettingsTestSA(t, s, project.ID, "sa-good")
	unverified := &store.GCPServiceAccount{
		ID: tid("sa-unverified-broker"), Scope: store.ScopeProject, ScopeID: project.ID,
		Email: "unverified-broker@proj.iam.gserviceaccount.com", ProjectID: "gcp-proj",
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), unverified))

	failedStatus := newSettingsTestSA(t, s, project.ID, "sa-failed-broker")
	failSAID = failedStatus.ID
	fault.Arm()

	otherProj := &store.Project{ID: tid("other-broker-project"), Name: "Other", Slug: "other-broker-project"}
	require.NoError(t, s.CreateProject(context.Background(), otherProj))
	otherSA := newSettingsTestSA(t, s, otherProj.ID, "sa-other-proj")

	url := "/api/v1/projects/" + project.ID + "/settings"

	// All three flat target types (docker, kubernetes, cloudrun) are accepted.
	rec := doRequest(t, srv, http.MethodPut, url, hubclient.ProjectSettings{
		DefaultGCPIdentityServiceAccountIDByBroker: map[string]string{
			flatDocker.ID: good.ID,
			flatK8s.ID:    good.ID,
			flatCR.ID:     good.ID,
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	// Clear before running rejection table.
	rec = doRequest(t, srv, http.MethodPut, url, map[string]interface{}{
		"defaultGCPIdentityServiceAccountIDByBroker": map[string]string{},
	})
	require.Equal(t, http.StatusOK, rec.Code)

	cases := []struct {
		name     string
		byBroker map[string]string
		wantCode string
		wantMsg  string
	}{
		{"unknown broker ID", map[string]string{"no-such-broker-id": good.ID}, ErrCodeInvalidRequest, "not found"},
		{"legacy profile-based broker", map[string]string{legacy.ID: good.ID}, ErrCodeInvalidRequest, "per-broker defaults apply only to flat Runtime Brokers"},
		{"empty broker ID", map[string]string{"": good.ID}, ErrCodeInvalidRequest, "is invalid"},
		{"padded broker ID", map[string]string{" " + flatDocker.ID: good.ID}, ErrCodeInvalidRequest, "is invalid"},
		{"empty SA ID", map[string]string{flatDocker.ID: ""}, ErrCodeInvalidRequest, "a service account ID is required"},
		{"unknown SA", map[string]string{flatDocker.ID: "no-such-sa"}, ErrCodeInvalidRequest, msgSANotAvailableInProject},
		{"cross-project SA", map[string]string{flatDocker.ID: otherSA.ID}, ErrCodeInvalidRequest, msgSANotAvailableInProject},
		{"unverified SA", map[string]string{flatDocker.ID: unverified.ID}, ErrCodeIdentityNotVerified, `as the default for Runtime Broker \"flat-docker\"`},
		{"verified flag with failed status", map[string]string{flatDocker.ID: failedStatus.ID}, ErrCodeIdentityNotVerified, `as the default for Runtime Broker \"flat-docker\"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPut, url, hubclient.ProjectSettings{
				DefaultGCPIdentityServiceAccountIDByBroker: tc.byBroker,
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
			apiErr := decodeFlatAPIError(t, rec)
			assert.Equal(t, tc.wantCode, apiErr.Code)
			assert.Contains(t, rec.Body.String(), tc.wantMsg)
			assert.Nil(t, getProjectSettings(t, srv, project.ID).DefaultGCPIdentityServiceAccountIDByBroker)
		})
	}

	// Unknown SA and cross-project SA produce the exact same message (no cross-project oracle).
	recUnknown := doRequest(t, srv, http.MethodPut, url, hubclient.ProjectSettings{
		DefaultGCPIdentityServiceAccountIDByBroker: map[string]string{flatDocker.ID: "no-such-sa"},
	})
	recCross := doRequest(t, srv, http.MethodPut, url, hubclient.ProjectSettings{
		DefaultGCPIdentityServiceAccountIDByBroker: map[string]string{flatDocker.ID: otherSA.ID},
	})
	assert.Equal(t, decodeFlatAPIError(t, recUnknown).Message, decodeFlatAPIError(t, recCross).Message)

	// Entry cap: 64 flat brokers accepted, 65th rejected.
	maxMap := make(map[string]string, maxBrokerDefaultSAEntries+1)
	for i := 0; i < maxBrokerDefaultSAEntries; i++ {
		b := createSettingsFlatBroker(t, s, fmt.Sprintf("cap-%d", i), fmt.Sprintf("flat-cap-%d", i), "docker")
		maxMap[b.ID] = good.ID
	}
	rec = doRequest(t, srv, http.MethodPut, url, hubclient.ProjectSettings{
		DefaultGCPIdentityServiceAccountIDByBroker: maxMap,
	})
	require.Equal(t, http.StatusOK, rec.Code, "64 entries must be accepted: %s", rec.Body.String())

	extraBroker := createSettingsFlatBroker(t, s, "cap-extra", "flat-cap-extra", "docker")
	maxMap[extraBroker.ID] = good.ID
	rec = doRequest(t, srv, http.MethodPut, url, hubclient.ProjectSettings{
		DefaultGCPIdentityServiceAccountIDByBroker: maxMap,
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "65 entries must be rejected: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "at most 64 entries")

	// Project-admin authorization: a plain project member is denied with 403.
	member := flatMemberUser(t, s, project.ID, "broker-default-nonadmin")
	rec = doRequestAsUser(t, srv, member, http.MethodPut, url, map[string]interface{}{
		"defaultGCPIdentityServiceAccountIDByBroker": map[string]string{flatDocker.ID: good.ID},
	})
	require.Equal(t, http.StatusForbidden, rec.Code, "non-admin member must not write per-broker defaults: %s", rec.Body.String())
}

type failedBrokerSAStatusStore struct {
	store.Store
	fault *storeFaultSwitch
	saID  *string
}

func (f *failedBrokerSAStatusStore) GetGCPServiceAccount(ctx context.Context, id string) (*store.GCPServiceAccount, error) {
	sa, err := f.Store.GetGCPServiceAccount(ctx, id)
	if f.fault.Active() && err == nil && sa != nil && f.saID != nil && id == *f.saID {
		cp := *sa
		cp.Verified = true
		cp.VerificationStatus = store.GCPVerificationFailed
		return &cp, nil
	}
	return sa, err
}

func TestProjectSettings_BrokerDefaultSA_DeletedBrokerCleanupAndReadIgnore(t *testing.T) {
	srv, s := testServer(t)
	setFlatExperiment(t, srv, true)
	project := createTestProjectForSettings(t, s)
	flatGone := createSettingsFlatBroker(t, s, "gone", "flat-gone", "cloudrun")
	flatKeep := createSettingsFlatBroker(t, s, "keep", "flat-keep", "docker")
	saOne := newSettingsTestSA(t, s, project.ID, "sa-del-1")
	saTwo := newSettingsTestSA(t, s, project.ID, "sa-del-2")
	url := "/api/v1/projects/" + project.ID + "/settings"

	rec := doRequest(t, srv, http.MethodPut, url, hubclient.ProjectSettings{
		DefaultGCPIdentityServiceAccountIDByBroker: map[string]string{flatGone.ID: saOne.ID},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Delete flatGone from the store after its per-broker default was saved.
	require.NoError(t, s.DeleteRuntimeBroker(context.Background(), flatGone.ID))

	// Read helper ignores a deleted broker's entry cleanly with a real non-nil Agent
	// (exercising GetRuntimeBroker -> ErrNotFound rather than short-circuiting on nil agent).
	projReloaded, err := s.GetProject(context.Background(), project.ID)
	require.NoError(t, err)
	realAgent := &store.Agent{
		ID:              tid("del-broker-agent"),
		Name:            "del-broker-agent",
		ProjectID:       project.ID,
		RuntimeBrokerID: flatGone.ID,
		AppliedConfig:   &store.AgentAppliedConfig{},
	}
	gotSA, err := srv.brokerDefaultSA(context.Background(), realAgent, flatGone.ID, projReloaded)
	require.NoError(t, err)
	assert.Empty(t, gotSA, "entry for a deleted broker must be ignored on read")

	// Also verify a legacy profile-based broker ID seeded directly in the annotation
	// is ignored at read (!broker.IsFlat()).
	legacy := &store.RuntimeBroker{
		ID:             tid("settings-legacy-read-ignore"),
		Name:           "legacy-read-ignore",
		Slug:           "legacy-read-ignore",
		Status:         store.BrokerStatusOnline,
		Profiles:       []store.BrokerProfile{{Name: "local", Type: "docker", Available: true}},
		DefaultProfile: "local",
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), legacy))
	projWithLegacyKey := *projReloaded
	rawWithLegacy, err := json.Marshal(map[string]string{
		flatGone.ID: saOne.ID,
		legacy.ID:   saOne.ID,
	})
	require.NoError(t, err)
	projWithLegacyKey.Annotations = map[string]string{
		projectSettingDefaultGCPIdentitySAIDByBroker: string(rawWithLegacy),
	}
	legacyAgent := &store.Agent{
		ID:              tid("legacy-broker-agent"),
		Name:            "legacy-broker-agent",
		ProjectID:       project.ID,
		RuntimeBrokerID: legacy.ID,
		AppliedConfig:   &store.AgentAppliedConfig{},
	}
	gotLegacySA, err := srv.brokerDefaultSA(context.Background(), legacyAgent, legacy.ID, &projWithLegacyKey)
	require.NoError(t, err)
	assert.Empty(t, gotLegacySA, "entry for a legacy non-flat broker must be ignored on read")

	// Unrelated partial PUT and adding another flat broker while keeping the unchanged stale entry succeed.
	rec = doRequest(t, srv, http.MethodPut, url, map[string]interface{}{"defaultModel": "gemini"})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	rec = doRequest(t, srv, http.MethodPut, url, hubclient.ProjectSettings{
		DefaultGCPIdentityServiceAccountIDByBroker: map[string]string{
			flatGone.ID: saOne.ID,
			flatKeep.ID: saTwo.ID,
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "adding a live broker while an unchanged deleted broker entry exists must succeed: %s", rec.Body.String())

	// Removing the deleted broker's entry succeeds.
	rec = doRequest(t, srv, http.MethodPut, url, hubclient.ProjectSettings{
		DefaultGCPIdentityServiceAccountIDByBroker: map[string]string{flatKeep.ID: saTwo.ID},
	})
	require.Equal(t, http.StatusOK, rec.Code, "removing a deleted broker entry must succeed: %s", rec.Body.String())
	assert.Equal(t, map[string]string{flatKeep.ID: saTwo.ID},
		getProjectSettings(t, srv, project.ID).DefaultGCPIdentityServiceAccountIDByBroker)

	// Clearing the entire map also succeeds.
	rec = doRequest(t, srv, http.MethodPut, url, map[string]interface{}{
		"defaultGCPIdentityServiceAccountIDByBroker": map[string]string{},
	})
	require.Equal(t, http.StatusOK, rec.Code, "clearing map to {} must succeed: %s", rec.Body.String())
	assert.Nil(t, getProjectSettings(t, srv, project.ID).DefaultGCPIdentityServiceAccountIDByBroker)
}

type brokerDefaultLadderFixture struct {
	*bypassAgentsFixture
	flatA     *store.RuntimeBroker
	flatB     *store.RuntimeBroker
	saBrokerA *store.GCPServiceAccount
	saProfile *store.GCPServiceAccount
	saProject *store.GCPServiceAccount
	saHub     *store.GCPServiceAccount
}

func newBrokerDefaultLadderFixture(t *testing.T) *brokerDefaultLadderFixture {
	t.Helper()
	f := bypassAgentsSetup(t)
	setFlatExperiment(t, f.srv, true)
	markBrokerStockProfiles(t, f, "remote")

	ctx := context.Background()
	flatA := &store.RuntimeBroker{
		ID:            tid("ladder-flat-a-" + t.Name()),
		Name:          "flat-cr-east",
		Slug:          "flat-cr-east",
		Status:        store.BrokerStatusOnline,
		AutoProvide:   true,
		CreatedBy:     f.owner.ID,
		Endpoint:      "http://flat-a.invalid",
		Capabilities:  &store.BrokerCapabilities{Reprovision: true, Sync: true},
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("ladder-target-a-" + t.Name()), Type: "docker", DisplayName: "Flat East"},
	}
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, flatA))
	require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: f.proj.ID, BrokerID: flatA.ID, BrokerName: flatA.Name,
		LocalPath: fixtureProviderCheckout(t), Status: store.BrokerStatusOnline,
	}))

	flatB := &store.RuntimeBroker{
		ID:            tid("ladder-flat-b-" + t.Name()),
		Name:          "flat-docker-west",
		Slug:          "flat-docker-west",
		Status:        store.BrokerStatusOnline,
		AutoProvide:   true,
		CreatedBy:     f.owner.ID,
		Endpoint:      "http://flat-b.invalid",
		Capabilities:  &store.BrokerCapabilities{Reprovision: true, Sync: true},
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("ladder-target-b-" + t.Name()), Type: "docker", DisplayName: "Flat West"},
	}
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, flatB))
	require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: f.proj.ID, BrokerID: flatB.ID, BrokerName: flatB.Name,
		LocalPath: fixtureProviderCheckout(t), Status: store.BrokerStatusOnline,
	}))

	bf := &brokerDefaultLadderFixture{
		bypassAgentsFixture: f,
		flatA:               flatA,
		flatB:               flatB,
		saBrokerA:           bypassAgentsCreateSA(t, f, f.proj.ID, true),
		saProfile:           bypassAgentsCreateSA(t, f, f.proj.ID, true),
		saProject:           bypassAgentsCreateSA(t, f, f.proj.ID, true),
		saHub:               bypassAgentsCreateSA(t, f, f.proj.ID, true),
	}
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().
		AllowTarget(bf.saBrokerA.Email).
		AllowTarget(bf.saProfile.Email).
		AllowTarget(bf.saProject.Email).
		AllowTarget(bf.saHub.Email))
	return bf
}

func (bf *brokerDefaultLadderFixture) updateAnnotations(t *testing.T, set map[string]string, remove ...string) {
	t.Helper()
	ctx := context.Background()
	p, err := bf.store.GetProject(ctx, bf.proj.ID)
	require.NoError(t, err)
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	for k, v := range set {
		p.Annotations[k] = v
	}
	for _, k := range remove {
		delete(p.Annotations, k)
	}
	require.NoError(t, bf.store.UpdateProject(ctx, p))
}

func (bf *brokerDefaultLadderFixture) setBrokerDefaults(t *testing.T, byBroker map[string]string) {
	t.Helper()
	raw, err := json.Marshal(byBroker)
	require.NoError(t, err)
	bf.updateAnnotations(t, map[string]string{projectSettingDefaultGCPIdentitySAIDByBroker: string(raw)})
}

func TestBrokerDefaultSA_InteractiveCreateLadder(t *testing.T) {
	bf := newBrokerDefaultLadderFixture(t)
	byProfileRaw, err := json.Marshal(map[string]string{"remote": bf.saProfile.ID})
	require.NoError(t, err)
	bf.updateAnnotations(t, map[string]string{
		projectSettingDefaultGCPIdentityMode:          store.GCPMetadataModeAssign,
		projectSettingDefaultGCPIdentitySAID:          bf.saProject.ID,
		projectSettingDefaultGCPIdentitySAIDByProfile: string(byProfileRaw),
	})
	// Seed flatA, a stale deleted-broker UUID, AND the legacy profile-based broker's own ID
	// directly in the annotation so read-side guards (!broker.IsFlat() and ErrNotFound) are exercised.
	bf.setBrokerDefaults(t, map[string]string{
		bf.flatA.ID:           bf.saBrokerA.ID,
		bf.broker.ID:          bf.saBrokerA.ID,
		"deleted-broker-uuid": bf.saBrokerA.ID,
	})
	setHubAgentDefaults(bf.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: bf.saHub.ID,
	})

	// 1. Flat broker A gets its per-broker default (winning over project and hub defaults) and records origin.
	logs := captureDefaultSlog(t)
	agentA := createdAgentRecord(t, bf.bypassAgentsFixture, CreateAgentRequest{
		Name:            "on-flat-a",
		RuntimeBrokerID: bf.flatA.ID,
	})
	assertAssigned(t, agentA, bf.saBrokerA, "per-broker default must apply on flatA")
	rowsA := activeAssignments(t, bf.store, agentA.ID)
	require.Len(t, rowsA, 1)
	assert.Equal(t, store.SAAssignmentOriginCreateProjectBrokerDefault, rowsA[0].Origin)
	recs := logs.recordsContaining("GCP identity chosen by default")
	require.NotEmpty(t, recs)
	assert.Equal(t, slog.LevelDebug, recs[len(recs)-1].Level)
	assert.Equal(t, "project-broker-default", logSource(recs[len(recs)-1]))

	// 2. Flat broker B (no entry in by-broker) falls through to the project default, ignoring by-profile.
	agentB := createdAgentRecord(t, bf.bypassAgentsFixture, CreateAgentRequest{
		Name:            "on-flat-b",
		RuntimeBrokerID: bf.flatB.ID,
	})
	assertAssigned(t, agentB, bf.saProject, "flatB without a by-broker entry must fall through to project default")
	rowsB := activeAssignments(t, bf.store, agentB.ID)
	require.Len(t, rowsB, 1)
	assert.Equal(t, store.SAAssignmentOriginCreateProjectDefault, rowsB[0].Origin)

	// 3. Legacy profile-based broker ignores even its own seeded by-broker entry (!broker.IsFlat())
	// and applies its per-profile default (or project default when by-profile is absent).
	agentLegacy := createdAgentRecord(t, bf.bypassAgentsFixture, CreateAgentRequest{
		Name:            "on-legacy",
		RuntimeBrokerID: bf.broker.ID,
	})
	assertAssigned(t, agentLegacy, bf.saProfile, "legacy broker must ignore seeded by-broker entry and apply per-profile default")

	bf.updateAnnotations(t, nil, projectSettingDefaultGCPIdentitySAIDByProfile)
	agentLegacyProj := createdAgentRecord(t, bf.bypassAgentsFixture, CreateAgentRequest{
		Name:            "on-legacy-project-default",
		RuntimeBrokerID: bf.broker.ID,
	})
	assertAssigned(t, agentLegacyProj, bf.saProject, "legacy broker without by-profile must ignore seeded by-broker entry and apply project default")
	bf.updateAnnotations(t, map[string]string{projectSettingDefaultGCPIdentitySAIDByProfile: string(byProfileRaw)})

	// 4. Explicit request GCPIdentity (assign and block) wins over per-broker default on flatA.
	explicitAssign := createdAgentRecord(t, bf.bypassAgentsFixture, CreateAgentRequest{
		Name:            "explicit-assign-flat-a",
		RuntimeBrokerID: bf.flatA.ID,
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: bf.saProject.ID,
		},
	})
	assertAssigned(t, explicitAssign, bf.saProject, "explicit assign must win over per-broker default")

	explicitBlock := createdAgentRecord(t, bf.bypassAgentsFixture, CreateAgentRequest{
		Name:            "explicit-block-flat-a",
		RuntimeBrokerID: bf.flatA.ID,
		GCPIdentity:     &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModeBlock},
	})
	require.NotNil(t, explicitBlock.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeBlock, explicitBlock.AppliedConfig.GCPIdentity.MetadataMode)

	// 5. Per-broker default wins over explicit project "block" and "passthrough".
	bf.updateAnnotations(t, map[string]string{projectSettingDefaultGCPIdentityMode: store.GCPMetadataModeBlock},
		projectSettingDefaultGCPIdentitySAID)
	overBlock := createdAgentRecord(t, bf.bypassAgentsFixture, CreateAgentRequest{
		Name:            "over-project-block",
		RuntimeBrokerID: bf.flatA.ID,
	})
	assertAssigned(t, overBlock, bf.saBrokerA, "per-broker default must win over project block")

	// 6. With no project default set, flatA gets per-broker default and flatB falls through to hub default.
	bf.updateAnnotations(t, nil, projectSettingDefaultGCPIdentityMode, projectSettingDefaultGCPIdentitySAID)
	overHub := createdAgentRecord(t, bf.bypassAgentsFixture, CreateAgentRequest{
		Name:            "flat-a-over-hub",
		RuntimeBrokerID: bf.flatA.ID,
	})
	assertAssigned(t, overHub, bf.saBrokerA, "per-broker default must win over hub default")
	hubFallback := createdAgentRecord(t, bf.bypassAgentsFixture, CreateAgentRequest{
		Name:            "flat-b-falls-to-hub",
		RuntimeBrokerID: bf.flatB.ID,
	})
	assertAssigned(t, hubFallback, bf.saHub, "flatB without project default must fall through to hub default")
}

func TestBrokerDefaultSA_InteractiveBrokenDefaultNeverFallsBack(t *testing.T) {
	bf := newBrokerDefaultLadderFixture(t)
	bf.updateAnnotations(t, map[string]string{
		projectSettingDefaultGCPIdentityMode: store.GCPMetadataModeAssign,
		projectSettingDefaultGCPIdentitySAID: bf.saProject.ID,
	})

	t.Run("unverified SA fails with identity_default_invalid naming broker", func(t *testing.T) {
		unverified := bypassAgentsCreateSA(t, bf.bypassAgentsFixture, bf.proj.ID, false)
		bf.setBrokerDefaults(t, map[string]string{bf.flatA.ID: unverified.ID})

		rec := createAgentAsOwner(t, bf.bypassAgentsFixture, CreateAgentRequest{
			Name:            "broken-unverified",
			RuntimeBrokerID: bf.flatA.ID,
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
		apiErr := decodeFlatAPIError(t, rec)
		assert.Equal(t, ErrCodeIdentityDefaultInvalid, apiErr.Code)
		assert.Contains(t, apiErr.Message, `project default for Runtime Broker "flat-cr-east" GCP service account is not verified`)
		assert.Contains(t, apiErr.Message, "defaultGCPIdentityServiceAccountIDByBroker")
	})

	t.Run("deleted SA fails with identity_default_invalid naming broker", func(t *testing.T) {
		bf.setBrokerDefaults(t, map[string]string{bf.flatA.ID: "deleted-sa-uuid"})

		rec := createAgentAsOwner(t, bf.bypassAgentsFixture, CreateAgentRequest{
			Name:            "broken-deleted",
			RuntimeBrokerID: bf.flatA.ID,
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
		apiErr := decodeFlatAPIError(t, rec)
		assert.Equal(t, ErrCodeIdentityDefaultInvalid, apiErr.Code)
		assert.Contains(t, apiErr.Message, `project default for Runtime Broker "flat-cr-east" GCP service account is not available`)
	})

	t.Run("creator lacking actAs on per-broker SA is denied 403 without falling back to allowed project SA", func(t *testing.T) {
		bf.setBrokerDefaults(t, map[string]string{bf.flatA.ID: bf.saBrokerA.ID})
		enforceSAAssign(bf.srv, store.NewFakeCallerPermissionChecker().
			AllowTarget(bf.saProject.Email).
			DenyTarget(bf.saBrokerA.Email, "missing actAs on broker SA"))

		rec := createAgentAsOwner(t, bf.bypassAgentsFixture, CreateAgentRequest{
			Name:            "broken-unauthorized",
			RuntimeBrokerID: bf.flatA.ID,
		})
		require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
		apiErr := decodeFlatAPIError(t, rec)
		assert.Equal(t, ErrCodeIdentityAssignDenied, apiErr.Code)
	})
}

func fireScheduledDispatchOnDefaultBroker(t *testing.T, bf *brokerDefaultLadderFixture, defaultBrokerID, agentName string) error {
	t.Helper()
	ctx := context.Background()
	p, err := bf.store.GetProject(ctx, bf.proj.ID)
	require.NoError(t, err)
	p.DefaultRuntimeBrokerID = defaultBrokerID
	require.NoError(t, bf.store.UpdateProject(ctx, p))
	return fireScheduledDispatchAsOwner(t, bf.bypassAgentsFixture, agentName)
}

func TestScheduledDispatch_BrokerDefaultSALadderAndFailClosed(t *testing.T) {
	bf := newBrokerDefaultLadderFixture(t)
	bf.updateAnnotations(t, map[string]string{
		projectSettingDefaultGCPIdentityMode: store.GCPMetadataModeAssign,
		projectSettingDefaultGCPIdentitySAID: bf.saProject.ID,
	})
	bf.setBrokerDefaults(t, map[string]string{
		bf.flatA.ID:           bf.saBrokerA.ID,
		bf.broker.ID:          bf.saBrokerA.ID,
		"deleted-broker-uuid": bf.saBrokerA.ID,
	})

	// 1. Scheduled dispatch onto flatA applies flatA's per-broker default and records scheduled origin.
	require.NoError(t, fireScheduledDispatchOnDefaultBroker(t, bf, bf.flatA.ID, "sched-flat-a"))
	gotA, err := bf.store.GetAgentBySlug(context.Background(), bf.proj.ID, "sched-flat-a")
	require.NoError(t, err)
	assertAssigned(t, gotA, bf.saBrokerA, "scheduled dispatch on flatA must apply per-broker default")
	rowsA := activeAssignments(t, bf.store, gotA.ID)
	require.Len(t, rowsA, 1)
	assert.Equal(t, store.SAAssignmentOriginScheduledProjectBrokerDefault, rowsA[0].Origin)

	// 2. Scheduled dispatch onto flatB (no by-broker entry) falls through to project default.
	require.NoError(t, fireScheduledDispatchOnDefaultBroker(t, bf, bf.flatB.ID, "sched-flat-b"))
	gotB, err := bf.store.GetAgentBySlug(context.Background(), bf.proj.ID, "sched-flat-b")
	require.NoError(t, err)
	assertAssigned(t, gotB, bf.saProject, "scheduled dispatch on flatB must fall through to project default")
	rowsB := activeAssignments(t, bf.store, gotB.ID)
	require.Len(t, rowsB, 1)
	assert.Equal(t, store.SAAssignmentOriginScheduledProjectDefault, rowsB[0].Origin)

	// 2b. Scheduled dispatch onto legacy profile-based broker ignores its own seeded by-broker entry (!broker.IsFlat())
	// and falls through to project default.
	require.NoError(t, fireScheduledDispatchOnDefaultBroker(t, bf, bf.broker.ID, "sched-legacy"))
	gotLegacy, err := bf.store.GetAgentBySlug(context.Background(), bf.proj.ID, "sched-legacy")
	require.NoError(t, err)
	assertAssigned(t, gotLegacy, bf.saProject, "scheduled dispatch on legacy broker must ignore seeded by-broker entry")

	// 3. Unverified SA in by-broker default fails scheduled dispatch closed (no fallback, no agent record).
	unverified := bypassAgentsCreateSA(t, bf.bypassAgentsFixture, bf.proj.ID, false)
	bf.setBrokerDefaults(t, map[string]string{bf.flatA.ID: unverified.ID})
	err = fireScheduledDispatchOnDefaultBroker(t, bf, bf.flatA.ID, "sched-flat-unverified")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `project default for Runtime Broker "flat-cr-east" GCP service account is not verified`)
	_, getErr := bf.store.GetAgentBySlug(context.Background(), bf.proj.ID, "sched-flat-unverified")
	assert.ErrorIs(t, getErr, store.ErrNotFound)

	// 4. Unauthorized SA in by-broker default fails scheduled dispatch closed without falling back.
	bf.setBrokerDefaults(t, map[string]string{bf.flatA.ID: bf.saBrokerA.ID})
	enforceSAAssign(bf.srv, store.NewFakeCallerPermissionChecker().
		AllowTarget(bf.saProject.Email).
		DenyTarget(bf.saBrokerA.Email, "no actAs grant"))
	err = fireScheduledDispatchOnDefaultBroker(t, bf, bf.flatA.ID, "sched-flat-denied")
	require.Error(t, err)
	assert.Contains(t, err.Error(), store.PermissionActAs)
	_, getErr = bf.store.GetAgentBySlug(context.Background(), bf.proj.ID, "sched-flat-denied")
	assert.ErrorIs(t, getErr, store.ErrNotFound)
}

type brokerDefaultTransientErrStore struct {
	store.Store
	fault     *storeFaultSwitch
	brokerID  string
	triggered atomic.Int32
}

func callerIncludesFunc(substr string) bool {
	var pcs [32]uintptr
	n := runtime.Callers(2, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if strings.Contains(frame.Function, substr) {
			return true
		}
		if !more {
			return false
		}
	}
}

func (f *brokerDefaultTransientErrStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	if f.fault.Active() && id == f.brokerID && callerIncludesFunc("brokerDefaultSA") {
		f.triggered.Add(1)
		return nil, errors.New("simulated transient GetRuntimeBroker store failure")
	}
	return f.Store.GetRuntimeBroker(ctx, id)
}

func TestBrokerDefaultSA_TransientGetRuntimeBrokerErrorFailsClosed(t *testing.T) {
	bf := newBrokerDefaultLadderFixture(t)
	bf.updateAnnotations(t, map[string]string{
		projectSettingDefaultGCPIdentityMode: store.GCPMetadataModeAssign,
		projectSettingDefaultGCPIdentitySAID: bf.saProject.ID,
	})
	bf.setBrokerDefaults(t, map[string]string{bf.flatA.ID: bf.saBrokerA.ID})
	setHubAgentDefaults(bf.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: bf.saHub.ID,
	})

	wrapped, fault := installStoreFault(t, bf.srv, func(inner store.Store, fault *storeFaultSwitch) *brokerDefaultTransientErrStore {
		return &brokerDefaultTransientErrStore{Store: inner, fault: fault, brokerID: bf.flatA.ID}
	})
	fault.Arm()

	// 1. Direct helper returns the transient store error instead of ("", nil).
	proj, err := bf.store.GetProject(context.Background(), bf.proj.ID)
	require.NoError(t, err)
	agent := &store.Agent{ID: "transient-agent", Name: "transient-agent", ProjectID: bf.proj.ID, RuntimeBrokerID: bf.flatA.ID, AppliedConfig: &store.AgentAppliedConfig{}}
	gotSA, err := bf.srv.brokerDefaultSA(context.Background(), agent, bf.flatA.ID, proj)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "simulated transient GetRuntimeBroker store failure")
	assert.Empty(t, gotSA)

	// 2. Interactive create fails closed with 500 without falling back to saProject/saHub or persisting an agent.
	wrapped.triggered.Store(0)
	rec := createAgentAsOwner(t, bf.bypassAgentsFixture, CreateAgentRequest{
		Name:            "transient-http-create",
		RuntimeBrokerID: bf.flatA.ID,
	})
	require.Equal(t, http.StatusInternalServerError, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, int32(1), wrapped.triggered.Load(), "fault must fire inside brokerDefaultSA")
	_, getErr := bf.store.GetAgentBySlug(context.Background(), bf.proj.ID, "transient-http-create")
	assert.ErrorIs(t, getErr, store.ErrNotFound, "no agent must be created under broader default on transient store error")

	// 3. Scheduled dispatch fails closed without falling back to saProject/saHub or persisting an agent.
	wrapped.triggered.Store(0)
	err = fireScheduledDispatchOnDefaultBroker(t, bf, bf.flatA.ID, "transient-sched-create")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "simulated transient GetRuntimeBroker store failure")
	assert.Equal(t, int32(1), wrapped.triggered.Load(), "fault must fire inside brokerDefaultSA")
	_, getErr = bf.store.GetAgentBySlug(context.Background(), bf.proj.ID, "transient-sched-create")
	assert.ErrorIs(t, getErr, store.ErrNotFound, "scheduled dispatch must not create an agent under broader default on transient store error")
}

func TestProjectClone_BrokerDefaultSARemapped(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	srcID := api.NewUUID()
	projSA := &store.GCPServiceAccount{
		ID: api.NewUUID(), Scope: store.ScopeProject, ScopeID: srcID,
		Email: "broker-clone@proj.iam.gserviceaccount.com", ProjectID: "gcp-proj", Verified: true, CreatedBy: DevUserID,
	}
	const hubSAID = "hub-scoped-broker-sa-id"
	byBroker, err := json.Marshal(map[string]string{"broker-flat-1": projSA.ID, "broker-flat-2": hubSAID})
	require.NoError(t, err)
	src := &store.Project{
		ID: srcID, Name: "Broker SA Source", Slug: "broker-sa-source", OwnerID: DevUserID, CreatedBy: DevUserID,
		Annotations: map[string]string{projectSettingDefaultGCPIdentitySAIDByBroker: string(byBroker)},
	}
	require.NoError(t, s.CreateProject(ctx, src))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, projSA))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]string{"name": "Broker SA Clone"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var clone store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

	cloneSAs, err := s.ListGCPServiceAccounts(ctx, store.GCPServiceAccountFilter{Scope: store.ScopeProject, ScopeID: clone.ID})
	require.NoError(t, err)
	require.Len(t, cloneSAs, 1)

	persisted, err := s.GetProject(ctx, clone.ID)
	require.NoError(t, err)
	got := brokerDefaultSAIDsFromAnnotations(persisted.Annotations)
	assert.Equal(t, cloneSAs[0].ID, got["broker-flat-1"], "project-scoped SA entry must remap to cloned SA ID")
	assert.Equal(t, hubSAID, got["broker-flat-2"], "hub-scoped SA entry must remain unchanged")
}

func makeBrokerJSONStatusErr(t *testing.T, status int, code, msg string, details map[string]interface{}) error {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"code":    code,
			"message": msg,
			"details": details,
		},
	})
	require.NoError(t, err)
	return &brokerStatusError{StatusCode: status, Body: string(body)}
}

func TestCreateAgent_Broker400IdentityRejectionAppendsDefaultRungAttribution(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	saBroker := newSettingsTestSA(t, f.s, f.project.ID, "sa-attr-broker")
	saProject := newSettingsTestSA(t, f.s, f.project.ID, "sa-attr-project")
	saHub := newSettingsTestSA(t, f.s, f.project.ID, "sa-attr-hub")

	putSettings := func(annotations map[string]string) {
		p, err := f.s.GetProject(context.Background(), f.project.ID)
		require.NoError(t, err)
		p.Annotations = annotations
		require.NoError(t, f.s.UpdateProject(context.Background(), p))
	}

	t.Run("flat Cloud Run 400 validation_error on per-broker default names per-broker setting", func(t *testing.T) {
		raw, err := json.Marshal(map[string]string{f.flat.ID: saBroker.ID})
		require.NoError(t, err)
		putSettings(map[string]string{projectSettingDefaultGCPIdentitySAIDByBroker: string(raw)})
		f.client.returnErr = makeBrokerJSONStatusErr(t, http.StatusBadRequest, ErrCodeValidationError,
			`agent "cr-agent": GCP identity mode "assign" on Cloud Run Instances: account not attachable on this broker`, nil)

		rec := f.create(t, map[string]interface{}{
			"name":            "cr-agent",
			"runtimeBrokerId": f.flat.ID,
		})
		requireAPIError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
		msg := decodeFlatAPIError(t, rec).Message
		assert.Contains(t, msg, "account not attachable on this broker")
		assert.Contains(t, msg, `GCP identity was set by the project default for Runtime Broker "flat-docker"`)
		assert.Contains(t, msg, "defaultGCPIdentityServiceAccountIDByBroker")
		assert.Contains(t, msg, "scion project identity default set|unset --broker "+f.flat.ID)
		assert.Nil(t, f.agentBySlug(t, "cr-agent"), "failed create must roll back agent row")
	})

	t.Run("flat Cloud Run E-10 400 validation_error on project block default names defaultGCPIdentityMode", func(t *testing.T) {
		putSettings(map[string]string{projectSettingDefaultGCPIdentityMode: store.GCPMetadataModeBlock})
		f.client.returnErr = makeBrokerJSONStatusErr(t, http.StatusBadRequest, ErrCodeValidationError,
			`agent "e10-agent": flat Cloud Run instances do not support per-agent GCP identities`, nil)

		rec := f.create(t, map[string]interface{}{
			"name":            "e10-agent",
			"runtimeBrokerId": f.flat.ID,
		})
		requireAPIError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
		msg := decodeFlatAPIError(t, rec).Message
		assert.Contains(t, msg, "flat Cloud Run instances do not support per-agent GCP identities")
		assert.Contains(t, msg, "GCP identity was set by the project default")
		assert.Contains(t, msg, "defaultGCPIdentityMode")
	})

	t.Run("flat Kubernetes 400 identity_not_mapped on per-broker default and hub default", func(t *testing.T) {
		raw, err := json.Marshal(map[string]string{f.flat.ID: saBroker.ID})
		require.NoError(t, err)
		putSettings(map[string]string{projectSettingDefaultGCPIdentitySAIDByBroker: string(raw)})
		f.client.returnErr = makeBrokerJSONStatusErr(t, http.StatusBadRequest, ErrCodeIdentityNotMapped,
			"unmapped", map[string]interface{}{
				api.BrokerErrDetailServiceAccount: saBroker.Email,
				api.BrokerErrDetailBroker:         f.flat.Name,
			})

		rec := f.create(t, map[string]interface{}{
			"name":            "k8s-broker-default",
			"runtimeBrokerId": f.flat.ID,
		})
		requireAPIError(t, rec, http.StatusBadRequest, ErrCodeIdentityNotMapped)
		msg := decodeFlatAPIError(t, rec).Message
		assert.Contains(t, msg, "has no Kubernetes service account mapping")
		assert.Contains(t, msg, `GCP identity was set by the project default for Runtime Broker "flat-docker"`)
		assert.Contains(t, msg, "defaultGCPIdentityServiceAccountIDByBroker")

		// Hub default attribution when project has no default.
		putSettings(nil)
		setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
			DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
			DefaultGCPIdentityServiceAccountID: saHub.ID,
		})
		t.Cleanup(func() { setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{}) })

		rec = f.create(t, map[string]interface{}{
			"name":            "k8s-hub-default",
			"runtimeBrokerId": f.flat.ID,
		})
		requireAPIError(t, rec, http.StatusBadRequest, ErrCodeIdentityNotMapped)
		msg = decodeFlatAPIError(t, rec).Message
		assert.Contains(t, msg, "GCP identity was set by the hub default")
		assert.Contains(t, msg, "agent_defaults.default_gcp_identity_service_account_id")
	})

	t.Run("explicit request GCPIdentity does NOT append default-rung attribution", func(t *testing.T) {
		raw, err := json.Marshal(map[string]string{f.flat.ID: saBroker.ID})
		require.NoError(t, err)
		putSettings(map[string]string{projectSettingDefaultGCPIdentitySAIDByBroker: string(raw)})
		f.client.returnErr = makeBrokerJSONStatusErr(t, http.StatusBadRequest, ErrCodeValidationError,
			`agent "explicit-agent": GCP identity mode "assign" on Cloud Run Instances: account not attachable on this broker`, nil)

		rec := f.create(t, map[string]interface{}{
			"name":            "explicit-agent",
			"runtimeBrokerId": f.flat.ID,
			"gcp_identity": map[string]interface{}{
				"metadata_mode":      store.GCPMetadataModeAssign,
				"service_account_id": saProject.ID,
			},
		})
		requireAPIError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
		msg := decodeFlatAPIError(t, rec).Message
		assert.Contains(t, msg, "account not attachable on this broker")
		assert.NotContains(t, msg, "GCP identity was set by", "explicit request identity must not blame a default rung")
	})

	t.Run("unrelated broker 400 validation_error does NOT append default-rung attribution", func(t *testing.T) {
		raw, err := json.Marshal(map[string]string{f.flat.ID: saBroker.ID})
		require.NoError(t, err)
		putSettings(map[string]string{projectSettingDefaultGCPIdentitySAIDByBroker: string(raw)})
		f.client.returnErr = makeBrokerJSONStatusErr(t, http.StatusBadRequest, ErrCodeValidationError,
			`invalid template configuration: unknown harness`, nil)

		rec := f.create(t, map[string]interface{}{
			"name":            "non-identity-err-agent",
			"runtimeBrokerId": f.flat.ID,
		})
		requireAPIError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
		msg := decodeFlatAPIError(t, rec).Message
		assert.Equal(t, "invalid template configuration: unknown harness", msg)
	})

	t.Run("generic Kubernetes ServiceAccount 400 validation_error does NOT append default-rung attribution", func(t *testing.T) {
		raw, err := json.Marshal(map[string]string{f.flat.ID: saBroker.ID})
		require.NoError(t, err)
		putSettings(map[string]string{projectSettingDefaultGCPIdentitySAIDByBroker: string(raw)})

		for i, brokerMsg := range []string{
			`pod spec serviceAccount "foo" invalid`,
			`template validation failed: service account "default" not found in namespace "agents"`,
		} {
			f.client.returnErr = makeBrokerJSONStatusErr(t, http.StatusBadRequest, ErrCodeValidationError, brokerMsg, nil)
			rec := f.create(t, map[string]interface{}{
				"name":            fmt.Sprintf("k8s-sa-err-agent-%d", i),
				"runtimeBrokerId": f.flat.ID,
			})
			requireAPIError(t, rec, http.StatusBadRequest, ErrCodeValidationError)
			msg := decodeFlatAPIError(t, rec).Message
			assert.Equal(t, brokerMsg, msg, "generic Kubernetes ServiceAccount error must not blame GCP default rung")
		}
	})
}
