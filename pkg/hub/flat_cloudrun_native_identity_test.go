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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestFlatCloudRunBroker_Predicate(t *testing.T) {
	assert.False(t, flatCloudRunBroker(nil), "nil broker")
	assert.False(t, flatCloudRunBroker(&store.RuntimeBroker{
		ID:       tid("legacy-cr"),
		Profiles: []store.BrokerProfile{{Name: "cr", Type: "cloudrun"}},
	}), "profile-based Cloud Run broker is not a flat Cloud Run broker")
	assert.False(t, flatCloudRunBroker(&store.RuntimeBroker{
		ID: tid("no-target"),
	}), "broker without RuntimeTarget")
	assert.False(t, flatCloudRunBroker(&store.RuntimeBroker{
		ID:            tid("flat-docker"),
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("t-docker"), Type: "docker"},
	}), "flat docker broker")
	assert.False(t, flatCloudRunBroker(&store.RuntimeBroker{
		ID:            tid("flat-k8s"),
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("t-k8s"), Type: "kubernetes"},
	}), "flat kubernetes broker")
	for _, nonCanon := range nonCanonicalCloudRunTypes() {
		assert.False(t, flatCloudRunBroker(&store.RuntimeBroker{
			ID:            tid("flat-noncanon"),
			RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("t-noncanon"), Type: nonCanon},
		}), "non-canonical spelling %q must not match", nonCanon)
	}

	assert.True(t, flatCloudRunBroker(&store.RuntimeBroker{
		ID:            tid("flat-cr"),
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("t-cr"), Type: "cloudrun"},
	}), "canonical flat cloudrun broker")
}

// nativeOrderCheckingClient wraps mockRuntimeBrokerClient and records whether
// NativeCloudRun was already persisted on the agent row in store before the
// outgoing broker RPC ran.
type nativeOrderCheckingClient struct {
	*mockRuntimeBrokerClient
	t                  *testing.T
	st                 store.Store
	agentID            string
	recordedBeforeCall bool
}

func (c *nativeOrderCheckingClient) snapshotNativeBeforeCall() {
	c.recordedBeforeCall = crRecorded(c.t, c.st, c.agentID)
}

func (c *nativeOrderCheckingClient) CreateAgent(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	c.snapshotNativeBeforeCall()
	return c.mockRuntimeBrokerClient.CreateAgent(ctx, brokerID, brokerEndpoint, req)
}

func (c *nativeOrderCheckingClient) CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	c.snapshotNativeBeforeCall()
	return c.mockRuntimeBrokerClient.CreateAgentWithGather(ctx, brokerID, brokerEndpoint, req)
}

func (c *nativeOrderCheckingClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace, resume bool, extras StartExtras) (*RemoteAgentResponse, error) {
	c.snapshotNativeBeforeCall()
	return c.mockRuntimeBrokerClient.StartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash, resolvedEnv, resolvedSecrets, inlineConfig, sharedDirs, sharedWorkspace, resume, extras)
}

func (c *nativeOrderCheckingClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, resolvedEnv map[string]string, extras StartExtras) (*RemoteAgentResponse, error) {
	c.snapshotNativeBeforeCall()
	return c.mockRuntimeBrokerClient.RestartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, resolvedEnv, extras)
}

func flatCloudRunDispatcher(t *testing.T, gcpID *store.GCPIdentityConfig, nativeExpOn bool) (*HTTPAgentDispatcher, *nativeOrderCheckingClient, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)
	brokerID := tid("flat-cr-host")
	targetID := tid("flat-cr-target")
	broker := &store.RuntimeBroker{
		ID:            brokerID,
		Name:          "flat-cr",
		Slug:          "flat-cr",
		Endpoint:      "http://localhost:9800",
		Status:        store.BrokerStatusOnline,
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: targetID, Type: "cloudrun", DisplayName: "Flat Cloud Run"},
	}
	require.NoError(t, memStore.CreateRuntimeBroker(ctx, broker))

	agentID := tid("agent-flat-cr")
	client := &nativeOrderCheckingClient{
		mockRuntimeBrokerClient: &mockRuntimeBrokerClient{},
		t:                       t,
		st:                      memStore,
		agentID:                 agentID,
	}
	d := NewHTTPAgentDispatcherWithClient(memStore, client, false, slog.Default())
	if nativeExpOn {
		d.SetDispatchExperimentsProvider(func() []string {
			return []string{experiments.FlatRuntimeBrokers, experiments.FlatCloudRunTarget, experiments.CloudRunNativeIdentity}
		})
	} else {
		d.SetDispatchExperimentsProvider(func() []string {
			return []string{experiments.FlatRuntimeBrokers, experiments.FlatCloudRunTarget}
		})
	}
	agent := &store.Agent{
		ID:                      agentID,
		Name:                    "flat-cr-agent",
		Slug:                    "flat-cr-agent",
		ProjectID:               tid("project-flat-cr"),
		RuntimeBrokerID:         brokerID,
		PinnedRuntimeBrokerID:   brokerID,
		PinnedRuntimeTargetID:   targetID,
		PinnedRuntimeTargetType: "cloudrun",
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			GCPIdentity:   gcpID,
		},
	}
	require.True(t, agent.PinValid())
	crPersist(t, d, agent)
	return d, client, agent
}

// Flat Cloud Run runtime targets are natively GCP-identity-assigned by target
// type, regardless of hub.cloudrun_native_identity:
//   - Hub profile-report precheck is skipped (no flat profile report exists).
//   - NativeCloudRun is persisted on the agent row BEFORE the broker RPC.
//   - The native marker (RemoteGCPIdentityConfig.NativeCloudRun on create when
//     GCPIdentity != nil, plus SCION_METADATA_NATIVE_CLOUDRUN=1 on every path)
//     is sent to the broker.
//   - Confirmed stop and confirmed delete clear NativeCloudRun; failed stop or
//     failed delete retains it fail-closed.
func TestFlatCloudRunNativeIdentity_DispatchAndLifecycle(t *testing.T) {
	ctx := context.Background()
	modes := []struct {
		name  string
		gcpID *store.GCPIdentityConfig
	}{
		{"unset", nil},
		{"block", crMode(store.GCPMetadataModeBlock)},
		{"confined_passthrough", &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModePassthrough, RequireLocalRuntime: true}},
		{"explicit_passthrough", crMode(store.GCPMetadataModePassthrough)},
		{"assign", crAssign(crUnlistedGSA)},
	}

	for _, gateOn := range []bool{false, true} {
		t.Run(fmt.Sprintf("cloudrun_native_identity=%t", gateOn), func(t *testing.T) {
			for _, mode := range modes {
				for _, op := range precheckOps(ctx) {
					t.Run(mode.name+"/"+op.name, func(t *testing.T) {
						d, c, a := flatCloudRunDispatcher(t, mode.gcpID, gateOn)
						err := op.run(d, a)
						_, isIdentity := identityMappingDispatchError(err)
						require.False(t, isIdentity, "Hub precheck must never refuse flat Cloud Run: %v", err)
						require.NoError(t, err)
						require.True(t, op.called(c.mockRuntimeBrokerClient), "broker must be called")
						assert.True(t, c.recordedBeforeCall, "NativeCloudRun must be persisted on the agent row BEFORE the broker RPC")
						assert.True(t, crRecorded(t, d.store, a.ID), "NativeCloudRun remains recorded after dispatch")
						assert.True(t, a.AppliedConfig.NativeCloudRun, "caller agent copy carries NativeCloudRun")

						field, env, envSet := crMarkerSent(t, op.name, c.mockRuntimeBrokerClient)
						if op.name != "start" && op.name != "restart" && mode.gcpID != nil {
							assert.True(t, field, "create path sets RemoteGCPIdentityConfig.NativeCloudRun")
						}
						assert.True(t, envSet, "SCION_METADATA_NATIVE_CLOUDRUN must be set on %s", op.name)
						assert.Equal(t, "1", env)
					})
				}
			}

			t.Run("stop_and_delete_clear_native_flag", func(t *testing.T) {
				d, c, a := flatCloudRunDispatcher(t, crMode(store.GCPMetadataModeBlock), gateOn)
				_, err := d.DispatchAgentCreate(ctx, a)
				require.NoError(t, err)
				require.True(t, crRecorded(t, d.store, a.ID))

				// Failed stop keeps NativeCloudRun fail-closed.
				c.returnErr = errors.New("broker unreachable")
				require.Error(t, d.DispatchAgentStop(ctx, a))
				assert.True(t, crRecorded(t, d.store, a.ID), "failed stop keeps NativeCloudRun")

				// Confirmed stop clears NativeCloudRun.
				c.returnErr = nil
				require.NoError(t, d.DispatchAgentStop(ctx, a))
				assert.True(t, c.stopCalled)
				assert.False(t, crRecorded(t, d.store, a.ID), "confirmed stop clears NativeCloudRun")
				assert.False(t, a.AppliedConfig.NativeCloudRun)

				// Re-dispatch start records NativeCloudRun again; failed delete keeps it; confirmed delete clears it.
				require.NoError(t, d.DispatchAgentStart(ctx, a, "task", false))
				require.True(t, crRecorded(t, d.store, a.ID))

				c.returnErr = errors.New("broker unreachable")
				require.Error(t, d.DispatchAgentDelete(ctx, a, false, false, false, time.Time{}))
				assert.True(t, crRecorded(t, d.store, a.ID), "failed delete keeps NativeCloudRun")

				c.returnErr = nil
				require.NoError(t, d.DispatchAgentDelete(ctx, a, false, false, false, time.Time{}))
				assert.True(t, c.deleteCalled)
				assert.False(t, crRecorded(t, d.store, a.ID), "confirmed delete clears NativeCloudRun")
			})
		})
	}

	t.Run("block_mode_reports_no_gcp_credentials_available", func(t *testing.T) {
		srv, _ := testServer(t)
		_, _, a := flatCloudRunDispatcher(t, crMode(store.GCPMetadataModeBlock), false)
		avail, err := srv.agentGCPCredentialsAvailable(ctx, a)
		require.NoError(t, err)
		assert.False(t, avail, "block mode on flat Cloud Run must not report ambient GCP credentials")
	})
}

func attachFlatBrokerToMintFixture(t *testing.T, f *gcpMintFixture, targetType, tag string) string {
	t.Helper()
	ctx := context.Background()
	brokerID := tid("flat-mint-" + tag)
	targetID := tid("flat-mint-target-" + tag)
	require.NoError(t, f.Store.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:            brokerID,
		Name:          "flat-" + tag,
		Slug:          "flat-" + tag,
		Endpoint:      "http://localhost:9800",
		Status:        store.BrokerStatusOnline,
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: targetID, Type: targetType, DisplayName: "Flat " + targetType},
	}))
	f.mutateAgent(t, func(a *store.Agent) {
		a.RuntimeBrokerID = brokerID
		a.PinnedRuntimeBrokerID = brokerID
		a.PinnedRuntimeTargetID = targetID
		a.PinnedRuntimeTargetType = targetType
		a.AppliedConfig.Profile = ""
	})
	return brokerID
}

// A flat Cloud Run agent has ScopeAgentGCPToken withheld, both Hub GCP mint
// endpoints refused with 403 (naming no service account), and gs:// link SA
// resolution withheld — whether hub.cloudrun_native_identity is off or on, and
// even if hub.flat_cloudrun_target is toggled off while an existing pinned
// agent is still assigned to the flat Cloud Run broker. Flat docker and
// kubernetes brokers remain non-native.
func TestFlatCloudRunNativeIdentity_TokensWithheldAndMintRefused(t *testing.T) {
	ctx := context.Background()

	for _, nativeExpOn := range []bool{false, true} {
		for _, flatCloudRunExpOn := range []bool{true, false} {
			name := fmt.Sprintf("native_exp=%t/flat_cr_exp=%t", nativeExpOn, flatCloudRunExpOn)
			t.Run(name, func(t *testing.T) {
				tag := fmt.Sprintf("cr-%t-%t", nativeExpOn, flatCloudRunExpOn)
				f := newGCPMintFixture(t, tag)
				gen := &countingGCPTokenGenerator{}
				f.Server.SetGCPTokenGenerator(gen)
				attachFlatBrokerToMintFixture(t, f, "cloudrun", tag)
				setExperimentOverrides(t, f.Server, map[string]bool{
					experiments.FlatRuntimeBrokers:     true,
					experiments.FlatCloudRunTarget:     flatCloudRunExpOn,
					experiments.CloudRunNativeIdentity: nativeExpOn,
				})

				assert.False(t, grantHasGCPScope(t, f), "flat Cloud Run agent must never receive GCP token scope")
				assert.NotEmpty(t, mustAuthorizedScopes(t, f), "standard agent role scopes remain")

				assertMintDenied(t, f)
				for _, tc := range []struct {
					path string
					body interface{}
				}{
					{"/api/v1/agent/gcp-token", nil},
					{"/api/v1/agent/gcp-identity-token", map[string]string{"audience": "https://example.com"}},
				} {
					rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, tc.path, tc.body, f.Token)
					require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
					assert.Contains(t, rec.Body.String(), "metadata server", tc.path)
					assert.NotContains(t, rec.Body.String(), f.SA.Email, tc.path)
					assert.NotContains(t, rec.Body.String(), "iam.gserviceaccount.com", tc.path)
				}
				assert.Zero(t, gen.calls, "no GCP token may be generated")

				sa, reason := f.Server.resolveCurrentSA(ctx, f.getAgent(t))
				assert.Nil(t, sa)
				assert.Equal(t, GCSLinkReasonNativeIdentity, reason)
			})
		}
	}

	for _, nonCRType := range []string{"docker", "kubernetes"} {
		t.Run("flat_"+nonCRType+"_keeps_hub_mint", func(t *testing.T) {
			f := newGCPMintFixture(t, "flat-"+nonCRType)
			attachFlatBrokerToMintFixture(t, f, nonCRType, nonCRType)
			setExperimentOverrides(t, f.Server, map[string]bool{
				experiments.FlatRuntimeBrokers:     true,
				experiments.FlatCloudRunTarget:     true,
				experiments.CloudRunNativeIdentity: true,
			})
			assert.True(t, grantHasGCPScope(t, f), "flat %s broker must keep Hub GCP token scope", nonCRType)
			assertMintAllowed(t, f)
			sa, reason := f.Server.resolveCurrentSA(ctx, f.getAgent(t))
			require.NotNil(t, sa)
			assert.Empty(t, reason)
		})
	}
}

// TestFlat_CloudRunHubRecordsOmitCloudFields verifies end to end from
// config.PresentInstance through Hub heartbeat storage and public/operator GET
// responses (/api/v1/runtime-brokers/{id} and /api/v1/runtime-brokers) that
// region and type are presented while service_account, block_service_account,
// assignable_service_accounts (both keys and SA emails), project_id, network,
// subnetwork, and credentials_file never appear on the Hub.
func TestFlat_CloudRunHubRecordsOmitCloudFields(t *testing.T) {
	f := newFlatRegFixture(t, true)
	setFlatExperiments(t, f.srv, true, true)
	grantDevUserRuntimeBrokerAccess(t, f.s)

	brokerID := tid("cr-present-broker")
	target := cloudRunTargetFor("present-broker")
	regRec := f.register(t, f.operator, CreateBrokerRegistrationRequest{
		BrokerID:      brokerID,
		Name:          "cr-west",
		RuntimeTarget: target,
	})
	require.Equal(t, http.StatusCreated, regRec.Code, regRec.Body.String())

	inst := config.V1RuntimeBrokerInstanceConfig{
		Key:  "cr-west",
		Name: "cr-west",
		RuntimeTarget: &config.V1RuntimeTargetConfig{
			Type:                "cloudrun",
			DisplayName:         "Cloud Run West",
			ProjectID:           "never-gcp-proj",
			Region:              "us-central1",
			ServiceAccount:      "never-default-sa@never-gcp-proj.iam.gserviceaccount.com",
			BlockServiceAccount: "never-block-sa@never-gcp-proj.iam.gserviceaccount.com",
			AssignableServiceAccounts: []string{
				"never-assign-a@never-gcp-proj.iam.gserviceaccount.com",
				"never-assign-b@never-gcp-proj.iam.gserviceaccount.com",
			},
			Network:         "never-vpc",
			Subnetwork:      "never-subnet",
			CredentialsFile: "/etc/never/key.json",
		},
	}
	pres := config.PresentInstance(inst).API()
	sendAsBroker(t, f.srv, brokerID, presentationBeat(&pres, target.ID, 1, 1, nil))

	stored, err := f.s.GetRuntimeBroker(context.Background(), brokerID)
	require.NoError(t, err)
	require.NotNil(t, stored.RuntimeTargetPresentation)
	require.NotNil(t, stored.RuntimeTargetPresentation.Target)
	assert.Equal(t, "cloudrun", stored.RuntimeTargetPresentation.Target.Type)
	assert.Equal(t, "us-central1", stored.RuntimeTargetPresentation.Target.Region)

	forbiddenSubstrings := []string{
		"never-gcp-proj",
		"never-default-sa",
		"never-block-sa",
		"never-assign-a",
		"never-assign-b",
		"iam.gserviceaccount.com",
		"never-vpc",
		"never-subnet",
		"/etc/never/key.json",
		"serviceAccount",
		"service_account",
		"blockServiceAccount",
		"block_service_account",
		"assignableServiceAccounts",
		"assignable_service_accounts",
		"credentialsFile",
		"credentials_file",
		"projectId",
		"project_id",
		"subnetwork",
	}

	storedRaw, err := json.Marshal(stored)
	require.NoError(t, err)
	for _, bad := range forbiddenSubstrings {
		assert.NotContains(t, string(storedRaw), bad, "stored RuntimeBroker JSON must not contain %q", bad)
	}

	for _, path := range []string{
		"/api/v1/runtime-brokers/" + brokerID,
		"/api/v1/runtime-brokers",
	} {
		rec := doRequest(t, f.srv, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		body := rec.Body.String()
		assert.Contains(t, body, `"us-central1"`, "%s should expose presented region", path)
		for _, bad := range forbiddenSubstrings {
			assert.NotContains(t, body, bad, "%s response must not contain %q", path, bad)
		}
	}
}
