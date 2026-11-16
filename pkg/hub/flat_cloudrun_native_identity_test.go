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
	"net/http/httptest"
	"strings"
	"sync"
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

	t.Run("gcp_credentials_available_matrix_with_verified_project_sa", func(t *testing.T) {
		srv, st := testServer(t)
		projID := tid("project-flat-cr-creds")
		require.NoError(t, st.CreateProject(ctx, &store.Project{
			ID: projID, Name: "flat-cr-creds", Slug: "flat-cr-creds", Created: time.Now(), Updated: time.Now(),
		}))
		require.NoError(t, st.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{
			ID: tid("sa-flat-cr-creds"), Scope: store.ScopeProject, ScopeID: projID,
			Email: crListedGSA, ProjectID: "example-project",
			Verified: true, VerifiedAt: time.Now(), VerificationStatus: store.GCPVerificationVerified,
			CreatedAt: time.Now(),
		}))
		flatBrokerID := tid("flat-cr-creds-broker")
		flatTargetID := tid("flat-cr-creds-target")
		require.NoError(t, st.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID:            flatBrokerID,
			Name:          "flat-cr-creds",
			Slug:          "flat-cr-creds",
			Endpoint:      "http://localhost:9800",
			Status:        store.BrokerStatusOnline,
			RuntimeTarget: &api.RuntimeTargetDescriptor{ID: flatTargetID, Type: "cloudrun", DisplayName: "Flat Cloud Run"},
		}))
		legacyBrokerID := tid("legacy-docker-creds-broker")
		require.NoError(t, st.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID:       legacyBrokerID,
			Name:     "legacy-docker-creds",
			Slug:     "legacy-docker-creds",
			Endpoint: "http://localhost:9801",
			Status:   store.BrokerStatusOnline,
			Profiles: []store.BrokerProfile{{Name: "local", Type: "docker", Available: true}},
		}))

		cases := []struct {
			name  string
			gcpID *store.GCPIdentityConfig
			want  bool
		}{
			{"assign", crAssign(crListedGSA), true},
			{"explicit_passthrough", crMode(store.GCPMetadataModePassthrough), true},
			{"confined_passthrough", &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModePassthrough, RequireLocalRuntime: true}, false},
			{"block", crMode(store.GCPMetadataModeBlock), false},
			{"unset_nil", nil, false},
			{"unset_empty_mode", &store.GCPIdentityConfig{}, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				// Test both unpinned (broker lookup) and pinned flat Cloud Run agent shapes.
				for _, pinned := range []bool{false, true} {
					a := &store.Agent{
						ID:              tid("agent-creds-" + tc.name),
						Name:            "agent-" + tc.name,
						Slug:            "agent-" + tc.name,
						ProjectID:       projID,
						RuntimeBrokerID: flatBrokerID,
						AppliedConfig:   &store.AgentAppliedConfig{GCPIdentity: tc.gcpID},
					}
					if pinned {
						a.PinnedRuntimeBrokerID = flatBrokerID
						a.PinnedRuntimeTargetID = flatTargetID
						a.PinnedRuntimeTargetType = "cloudrun"
					}
					avail, err := srv.agentGCPCredentialsAvailable(ctx, a)
					require.NoError(t, err)
					assert.Equal(t, tc.want, avail, "pinned=%t mode=%s", pinned, tc.name)
				}
			})
		}

		// Positive control: a non-flat-Cloud-Run agent in the same project still
		// counts the verified project SA when GCPIdentity is nil.
		legacyAgent := &store.Agent{
			ID:              tid("agent-creds-legacy"),
			Name:            "agent-legacy",
			Slug:            "agent-legacy",
			ProjectID:       projID,
			RuntimeBrokerID: legacyBrokerID,
			AppliedConfig:   &store.AgentAppliedConfig{},
		}
		avail, err := srv.agentGCPCredentialsAvailable(ctx, legacyAgent)
		require.NoError(t, err)
		assert.True(t, avail, "non-flat-Cloud-Run agent with nil GCPIdentity counts verified project SA")
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
					assert.Contains(t, rec.Body.String(), "the hub does not issue GCP tokens for agents on Cloud Run Instances; use the instance's metadata server", tc.path)
					assert.NotContains(t, rec.Body.String(), "profile", tc.path)
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

// TestFlatCloudRunNativeIdentity_EndToEndHubHTTPAssignPassthroughAndRefusal
// exercises Hub HTTP create/start/restart/stop end-to-end against an HTTP flat
// Cloud Run broker server with hub.cloudrun_native_identity OFF and ON:
//   - allow-listed assign -> 201 Created, NativeCloudRun persisted, native
//     marker sent on create/start/restart, ScopeAgentGCPToken withheld on the
//     minted agent JWT, /api/v1/agent/gcp-token refused with 403;
//   - unlisted assign -> broker 400 validation_error naming
//     server.broker.instances[<key>].runtime_target.assignable_service_accounts
//     relayed cleanly without Kubernetes mapping prose, without SA email leak,
//     and without leaving a stranded agent or NativeCloudRun state;
//   - explicit passthrough -> 201 Created, NativeCloudRun persisted, native
//     marker sent, ScopeAgentGCPToken withheld, /api/v1/agent/gcp-token 403.
func TestFlatCloudRunNativeIdentity_EndToEndHubHTTPAssignPassthroughAndRefusal(t *testing.T) {
	ctx := context.Background()

	type capturedBrokerCall struct {
		method string
		path   string
		body   map[string]any
	}

	newFlatCRBrokerHTTP := func(t *testing.T, instanceKey, expectedTargetID string, assignable []string) (*httptest.Server, func() []capturedBrokerCall) {
		t.Helper()
		allow := map[string]bool{}
		for _, sa := range assignable {
			allow[strings.ToLower(strings.TrimSpace(sa))] = true
		}
		var mu sync.Mutex
		var calls []capturedBrokerCall

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			if r.Body != nil {
				_ = json.NewDecoder(r.Body).Decode(&body)
			}
			mu.Lock()
			calls = append(calls, capturedBrokerCall{method: r.Method, path: r.URL.Path, body: body})
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents":
				name, _ := body["name"].(string)
				if gotTarget, _ := body["expectedRuntimeTargetId"].(string); gotTarget != expectedTargetID {
					w.WriteHeader(http.StatusConflict)
					_ = json.NewEncoder(w).Encode(map[string]any{
						"error": map[string]any{"code": ErrCodeRuntimeTargetMismatch, "message": "target mismatch"},
					})
					return
				}
				cfg, _ := body["config"].(map[string]any)
				gcp, _ := cfg["gcpIdentity"].(map[string]any)
				mode, _ := gcp["metadata_mode"].(string)
				nativeMarked, _ := gcp["native_cloud_run"].(bool)
				if mode == store.GCPMetadataModeAssign {
					if !nativeMarked {
						w.WriteHeader(http.StatusBadRequest)
						_ = json.NewEncoder(w).Encode(map[string]any{
							"error": map[string]any{
								"code":    ErrCodeValidationError,
								"message": fmt.Sprintf("agent %q: the hub did not mark this dispatch for native Cloud Run identity", name),
							},
						})
						return
					}
					saEmail, _ := gcp["sa_email"].(string)
					if !allow[strings.ToLower(strings.TrimSpace(saEmail))] {
						w.WriteHeader(http.StatusBadRequest)
						_ = json.NewEncoder(w).Encode(map[string]any{
							"error": map[string]any{
								"code": ErrCodeValidationError,
								"message": fmt.Sprintf(
									"agent %q: GCP identity mode %q on Cloud Run Instances: service_account is not attachable on flat Cloud Run instance %q; add it to server.broker.instances[%s].runtime_target.assignable_service_accounts",
									name, store.GCPMetadataModeAssign, instanceKey, instanceKey),
							},
						})
						return
					}
				}
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"agent": map[string]any{
						"id": body["id"], "name": name, "slug": name, "phase": "running", "status": "running",
					},
				})
			case r.Method == http.MethodPost && (strings.HasSuffix(r.URL.Path, "/start") || strings.HasSuffix(r.URL.Path, "/restart")):
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"agent": map[string]any{"phase": "running", "status": "running"},
				})
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "stopped"})
			case r.Method == http.MethodDelete:
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{"deleted": true})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)
		return srv, func() []capturedBrokerCall {
			mu.Lock()
			defer mu.Unlock()
			out := make([]capturedBrokerCall, len(calls))
			copy(out, calls)
			return out
		}
	}

	for _, nativeExpOn := range []bool{false, true} {
		t.Run(fmt.Sprintf("cloudrun_native_identity=%t", nativeExpOn), func(t *testing.T) {
			f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
			cr := f.addFlatCloudRunBroker(t, "cloudrun")
			brokerSrv, getCalls := newFlatCRBrokerHTTP(t, "cr-a", cr.RuntimeTarget.ID, []string{crListedGSA})
			cr.Endpoint = brokerSrv.URL
			require.NoError(t, f.s.UpdateRuntimeBroker(ctx, cr))

			dispatcher := NewHTTPAgentDispatcher(f.s, false, slog.Default())
			dispatcher.SetTokenGenerator(f.srv)
			dispatcher.SetDispatchExperimentsProvider(f.srv.dispatchExperiments)
			f.srv.SetDispatcher(dispatcher)
			f.srv.SetGCPTokenGenerator(&countingGCPTokenGenerator{})

			setExperimentOverrides(t, f.srv, map[string]bool{
				experiments.FlatRuntimeBrokers:     true,
				experiments.FlatCloudRunTarget:     true,
				experiments.CloudRunNativeIdentity: nativeExpOn,
			})

			listedSA := &store.GCPServiceAccount{
				ID: tid(fmt.Sprintf("sa-listed-%t", nativeExpOn)), Scope: store.ScopeProject, ScopeID: f.project.ID,
				Email: crListedGSA, ProjectID: "example-project",
				Verified: true, VerifiedAt: time.Now(), VerificationStatus: store.GCPVerificationVerified,
				CreatedAt: time.Now(),
			}
			require.NoError(t, f.s.CreateGCPServiceAccount(ctx, listedSA))

			unlistedSA := &store.GCPServiceAccount{
				ID: tid(fmt.Sprintf("sa-unlisted-%t", nativeExpOn)), Scope: store.ScopeProject, ScopeID: f.project.ID,
				Email: crUnlistedGSA, ProjectID: "example-project",
				Verified: true, VerifiedAt: time.Now(), VerificationStatus: store.GCPVerificationVerified,
				CreatedAt: time.Now(),
			}
			require.NoError(t, f.s.CreateGCPServiceAccount(ctx, unlistedSA))

			// 1. Unlisted assign SA -> broker 400 validation_error naming
			// server.broker.instances[cr-a].runtime_target.assignable_service_accounts
			// relayed verbatim without Kubernetes prose, SA leak, or stranded row.
			recUnlisted := f.create(t, map[string]any{
				"name":            "cr-assign-unlisted",
				"runtimeBrokerId": cr.ID,
				"task":            "t",
				"noAuth":          true,
				"gcp_identity": map[string]any{
					"metadata_mode":      store.GCPMetadataModeAssign,
					"service_account_id": unlistedSA.ID,
				},
			})
			requireAPIError(t, recUnlisted, http.StatusBadRequest, ErrCodeValidationError)
			assert.Contains(t, recUnlisted.Body.String(), "server.broker.instances[cr-a].runtime_target.assignable_service_accounts")
			assert.NotContains(t, recUnlisted.Body.String(), "kubernetes_service_account_mappings")
			assert.NotContains(t, recUnlisted.Body.String(), "@")
			f.noAgentWritten(t, "cr-assign-unlisted")

			// 2. Allow-listed assign SA -> 201 Created + stop/start/restart + 403 mint refusal.
			recAssign := f.create(t, map[string]any{
				"name":            "cr-assign-ok",
				"runtimeBrokerId": cr.ID,
				"task":            "t",
				"noAuth":          true,
				"gcp_identity": map[string]any{
					"metadata_mode":      store.GCPMetadataModeAssign,
					"service_account_id": listedSA.ID,
				},
			})
			require.Equal(t, http.StatusCreated, recAssign.Code, recAssign.Body.String())
			assignAgent := f.agentBySlug(t, "cr-assign-ok")
			require.NotNil(t, assignAgent)
			assert.True(t, crRecorded(t, f.s, assignAgent.ID), "NativeCloudRun persisted on allow-listed assign")

			calls := getCalls()
			var createCall *capturedBrokerCall
			for i := range calls {
				if calls[i].method == http.MethodPost && calls[i].path == "/api/v1/agents" {
					if name, _ := calls[i].body["name"].(string); name == "cr-assign-ok" {
						createCall = &calls[i]
					}
				}
			}
			require.NotNil(t, createCall)
			cfgMap, _ := createCall.body["config"].(map[string]any)
			gcpMap, _ := cfgMap["gcpIdentity"].(map[string]any)
			assert.Equal(t, store.GCPMetadataModeAssign, gcpMap["metadata_mode"])
			assert.Equal(t, crListedGSA, gcpMap["sa_email"])
			assert.Equal(t, true, gcpMap["native_cloud_run"])
			resEnv, _ := createCall.body["resolvedEnv"].(map[string]any)
			assert.Equal(t, "1", resEnv[envNativeCloudRun])
			mintedToken, _ := createCall.body["agentToken"].(string)
			require.NotEmpty(t, mintedToken)

			mintRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agent/gcp-token", nil, mintedToken)
			require.Equal(t, http.StatusForbidden, mintRec.Code, mintRec.Body.String())
			assert.Contains(t, mintRec.Body.String(), "the hub does not issue GCP tokens for agents on Cloud Run Instances; use the instance's metadata server")
			assert.NotContains(t, mintRec.Body.String(), "profile")
			assert.NotContains(t, mintRec.Body.String(), "@")

			// Stop clears NativeCloudRun; Start and Restart re-record and send SCION_METADATA_NATIVE_CLOUDRUN=1.
			stopRec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+assignAgent.ID+"/stop", nil)
			require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, stopRec.Code, stopRec.Body.String())
			assert.False(t, crRecorded(t, f.s, assignAgent.ID))

			startRec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+assignAgent.ID+"/start", map[string]any{})
			require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, startRec.Code, startRec.Body.String())
			assert.True(t, crRecorded(t, f.s, assignAgent.ID))

			restartRec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+assignAgent.ID+"/restart", map[string]any{})
			require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, restartRec.Code, restartRec.Body.String())
			assert.True(t, crRecorded(t, f.s, assignAgent.ID))

			// Both lifecycle /start and lifecycle /restart (implemented as stop + start
			// in handleAgentLifecycle) dispatch /start to the broker with the native marker.
			var startCalls []capturedBrokerCall
			for _, c := range getCalls() {
				if c.method == http.MethodPost && strings.HasSuffix(c.path, "/cr-assign-ok/start") {
					startCalls = append(startCalls, c)
				}
			}
			require.Len(t, startCalls, 2, "expected broker /start calls for both lifecycle start and lifecycle restart")
			for idx, sc := range startCalls {
				re, _ := sc.body["resolvedEnv"].(map[string]any)
				assert.Equal(t, "1", re[envNativeCloudRun], "start call %d", idx)
				assert.Equal(t, store.GCPMetadataModeAssign, re["SCION_METADATA_MODE"], "start call %d", idx)
				assert.Equal(t, crListedGSA, re["SCION_METADATA_SA_EMAIL"], "start call %d", idx)
			}

			// 3. Explicit passthrough:
			//    3a. Without GCPHostServiceAccountEmail registered on the broker row,
			//        existing Hub passthrough rules (authorizePassthroughIdentity) deny
			//        with 403 Forbidden.
			recPTNoHostSA := f.create(t, map[string]any{
				"name":            "cr-passthrough-no-host-sa",
				"runtimeBrokerId": cr.ID,
				"task":            "t",
				"noAuth":          true,
				"gcp_identity": map[string]any{
					"metadata_mode": store.GCPMetadataModePassthrough,
				},
			})
			require.Equal(t, http.StatusForbidden, recPTNoHostSA.Code, recPTNoHostSA.Body.String())
			f.noAgentWritten(t, "cr-passthrough-no-host-sa")

			//    3b. With GCPHostServiceAccountEmail registered on the broker row ->
			//        201 Created, NativeCloudRun persisted, ScopeAgentGCPToken withheld,
			//        /api/v1/agent/gcp-token 403.
			_, err := f.s.SetRuntimeBrokerGCPHost(ctx, cr.ID, "cr-instance@example-project.iam.gserviceaccount.com", "example-project")
			require.NoError(t, err)

			recPT := f.create(t, map[string]any{
				"name":            "cr-passthrough-ok",
				"runtimeBrokerId": cr.ID,
				"task":            "t",
				"noAuth":          true,
				"gcp_identity": map[string]any{
					"metadata_mode": store.GCPMetadataModePassthrough,
				},
			})
			require.Equal(t, http.StatusCreated, recPT.Code, recPT.Body.String())
			ptAgent := f.agentBySlug(t, "cr-passthrough-ok")
			require.NotNil(t, ptAgent)
			assert.True(t, crRecorded(t, f.s, ptAgent.ID), "NativeCloudRun persisted on explicit passthrough")

			var ptCall *capturedBrokerCall
			for i := range getCalls() {
				c := getCalls()[i]
				if c.method == http.MethodPost && c.path == "/api/v1/agents" {
					if name, _ := c.body["name"].(string); name == "cr-passthrough-ok" {
						ptCall = &c
					}
				}
			}
			require.NotNil(t, ptCall)
			ptCfg, _ := ptCall.body["config"].(map[string]any)
			ptGCP, _ := ptCfg["gcpIdentity"].(map[string]any)
			assert.Equal(t, store.GCPMetadataModePassthrough, ptGCP["metadata_mode"])
			assert.Equal(t, true, ptGCP["native_cloud_run"])
			ptToken, _ := ptCall.body["agentToken"].(string)
			require.NotEmpty(t, ptToken)
			ptMintRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agent/gcp-token", nil, ptToken)
			require.Equal(t, http.StatusForbidden, ptMintRec.Code, ptMintRec.Body.String())
		})
	}
}
