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

package runtimebroker

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"github.com/googleapis/gax-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	flatCRTestInstanceSA = "instance-sa@example-project.iam.gserviceaccount.com"
	flatCRTestBlockSA    = "no-role-block@example-project.iam.gserviceaccount.com"
	flatCRTestAssignSA   = "worker-assign@example-project.iam.gserviceaccount.com"
	flatCRTestProjectID  = "9a1b2c3d-4e5f-4a6b-8c7d-0e1f2a3b4c5d"
)

func TestFlatCloudRunIdentityPolicy_SnapshotAndIsolation(t *testing.T) {
	target := &config.V1RuntimeTargetConfig{
		Type:                config.RuntimeTargetTypeCloudRun,
		ServiceAccount:      "  Instance-SA@Example-Project.iam.gserviceaccount.com ",
		BlockServiceAccount: "  No-Role-Block@Example-Project.iam.gserviceaccount.com ",
		AssignableServiceAccounts: []string{
			"  Worker-Assign@Example-Project.iam.gserviceaccount.com ",
			"",
		},
	}
	fi := &FlatInstanceConfig{
		Instance: config.V1RuntimeBrokerInstanceConfig{Key: "cr-a", Name: "cr-a", RuntimeTarget: target},
		Identity: &brokeridentity.Identity{
			InstanceKey:   "cr-a",
			RuntimeTarget: api.RuntimeTargetDescriptor{ID: "target-cr-a", Type: brokeridentity.TargetTypeCloudRun},
		},
	}
	p := newFlatCloudRunIdentityPolicy(fi)

	// Mutating the source config after snapshot construction must not alter p.
	target.ServiceAccount = "mutated@example-project.iam.gserviceaccount.com"
	target.BlockServiceAccount = "mutated-block@example-project.iam.gserviceaccount.com"
	target.AssignableServiceAccounts[0] = "mutated-assign@example-project.iam.gserviceaccount.com"

	blockSA, ok := p.blockAccount()
	require.True(t, ok)
	assert.Equal(t, flatCRTestBlockSA, blockSA)
	assert.Equal(t, flatCRTestInstanceSA, p.serviceAccount)
	assert.Equal(t, map[string]struct{}{flatCRTestAssignSA: {}}, p.assignable)

	// Empty / nil inputs fail closed.
	empty := newFlatCloudRunIdentityPolicy(nil)
	_, ok = empty.blockAccount()
	assert.False(t, ok)

	// Unvalidated server without block_service_account fails closed before side effects,
	// naming server.broker.instances[<key>].runtime_target.block_service_account and no account.
	srv := newFlatCloudRunStartServer(t)
	srv.config.FlatInstance.Instance.RuntimeTarget.BlockServiceAccount = ""
	srv.flatCloudRunIdentity = newFlatCloudRunIdentityPolicy(srv.config.FlatInstance)
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-no-block",
		HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	require.Nil(t, sc)
	sce := requireStartContextError(t, err)
	assert.Equal(t, http.StatusBadRequest, sce.Status)
	assert.Contains(t, sce.Message, "server.broker.instances[cr-a].runtime_target.block_service_account")
	assert.NotContains(t, sce.Message, "@")
}

// crLifecycleInstances is a stateful fake cloudrun.InstancesAPI supporting
// CreateInstance, GetInstance, ListInstances, StopInstance, DeleteInstance,
// and tracking StartInstance (which must remain 0 under native identity).
type crLifecycleInstances struct {
	mu      sync.Mutex
	gen     int64
	live    map[string]*runpb.Instance
	created []*runpb.Instance
	started []string
	stopped []string
	deleted []string
}

func newCRLifecycleInstances() *crLifecycleInstances {
	return &crLifecycleInstances{live: map[string]*runpb.Instance{}}
}

type crReadyOp struct{ inst *runpb.Instance }

func (op crReadyOp) Wait(context.Context, ...gax.CallOption) (*runpb.Instance, error) {
	if op.inst == nil {
		return nil, nil
	}
	return proto.Clone(op.inst).(*runpb.Instance), nil
}

func (c *crLifecycleInstances) GetInstance(_ context.Context, req *runpb.GetInstanceRequest, _ ...gax.CallOption) (*runpb.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	inst, ok := c.live[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "instance not found")
	}
	return proto.Clone(inst).(*runpb.Instance), nil
}

func (c *crLifecycleInstances) CreateInstance(_ context.Context, req *runpb.CreateInstanceRequest, _ ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	name := fmt.Sprintf("%s/instances/%s", req.GetParent(), req.GetInstanceId())
	if _, exists := c.live[name]; exists {
		return nil, status.Error(codes.AlreadyExists, "instance already exists")
	}
	c.gen++
	stored := proto.Clone(req.GetInstance()).(*runpb.Instance)
	stored.Name = name
	stored.Etag = fmt.Sprintf("etag-%d", c.gen)
	stored.Generation = c.gen
	stored.ObservedGeneration = c.gen
	stored.TerminalCondition = &runpb.Condition{State: runpb.Condition_CONDITION_SUCCEEDED}
	c.live[name] = stored
	c.created = append(c.created, proto.Clone(stored).(*runpb.Instance))
	return crReadyOp{inst: stored}, nil
}

func (c *crLifecycleInstances) StartInstance(_ context.Context, req *runpb.StartInstanceRequest, _ ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = append(c.started, req.GetName())
	return nil, status.Error(codes.FailedPrecondition, "StartInstance must never be called for native Cloud Run identity")
}

func (c *crLifecycleInstances) StopInstance(_ context.Context, req *runpb.StopInstanceRequest, _ ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	inst, ok := c.live[req.GetName()]
	if !ok {
		return nil, status.Error(codes.NotFound, "instance not found")
	}
	inst.TerminalCondition = &runpb.Condition{State: runpb.Condition_CONDITION_FAILED}
	c.stopped = append(c.stopped, req.GetName())
	return crReadyOp{}, nil
}

func (c *crLifecycleInstances) DeleteInstance(_ context.Context, req *runpb.DeleteInstanceRequest, _ ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.live[req.GetName()]; !ok {
		return nil, status.Error(codes.NotFound, "instance not found")
	}
	delete(c.live, req.GetName())
	c.deleted = append(c.deleted, req.GetName())
	return crReadyOp{}, nil
}

func (c *crLifecycleInstances) ListInstances(context.Context, *runpb.ListInstancesRequest, ...gax.CallOption) cloudrun.InstanceIterator {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*runpb.Instance, 0, len(c.live))
	for _, inst := range c.live {
		out = append(out, proto.Clone(inst).(*runpb.Instance))
	}
	return &crIter{instances: out}
}

func (c *crLifecycleInstances) Close() error { return nil }

func crInstanceEnvMap(inst *runpb.Instance) map[string]string {
	out := map[string]string{}
	if len(inst.GetContainers()) == 0 {
		return out
	}
	for _, ev := range inst.GetContainers()[0].GetEnv() {
		out[ev.GetName()] = ev.GetValue()
	}
	return out
}

func newFlatCloudRunLifecycleServer(t *testing.T) (*Server, *crLifecycleInstances, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	origWd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	global := filepath.Join(home, ".scion")
	nfsMountRoot := filepath.Join(t.TempDir(), "nfs")
	require.NoError(t, os.MkdirAll(filepath.Join(nfsMountRoot, "primary", "projects"), 0o755))
	// Write global settings with NFS workspace storage and a different legacy
	// Cloud Run block account to prove flat Cloud Run never consults
	// runtimes.<entry>.cloudrun.
	files := map[string]string{
		filepath.Join(global, "harness-configs", "generic", "config.yaml"): "harness: generic\nuser: scion\nimage: test-image:generic\n",
		filepath.Join(global, "templates", "default", "scion-agent.yaml"):  "harness_config: generic\n",
		filepath.Join(global, "settings.yaml"): `schema_version: "1"
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: ` + nfsMountRoot + `
      shares:
        - id: primary
          server: 10.0.0.2
          export: /vol-a
runtimes:
  cloudrun:
    type: cloudrun
    cloudrun:
      project_id: wrong-global-project
      service_account: wrong-global-sa@example-project.iam.gserviceaccount.com
      block_service_account: wrong-global-block@example-project.iam.gserviceaccount.com
`,
	}
	for p, body := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}

	fake := newCRLifecycleInstances()
	rt, err := runtime.NewCloudRunRuntimeForTest(
		&config.CloudRunConfig{
			ProjectID:      "example-project",
			Location:       "us-central1",
			ServiceAccount: flatCRTestInstanceSA,
			NFSServer:      "10.0.0.2",
			NFSExport:      "/vol-a",
		},
		func(context.Context) (cloudrun.InstancesAPI, error) { return fake, nil },
	)
	require.NoError(t, err)
	rt.StubNFSProvisionForTest("10.0.0.2", "/vol-a")

	const targetID = "target-flat-cr-a"
	cfg := DefaultServerConfig()
	cfg.Host, cfg.Port = "127.0.0.1", 0
	cfg.BrokerID = "rb-flat-cr-a"
	cfg.BrokerName = "cr-a"
	cfg.StateDir = t.TempDir()
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://127.0.0.1:1"
	cfg.ContainerHubEndpoint = "https://hub.example.com"
	cfg.BrokerAuthStrictMode = false
	cfg.InMemoryCredentials = &brokercredentials.BrokerCredentials{
		BrokerID:    "rb-flat-cr-a",
		SecretKey:   "c2VjcmV0",
		HubEndpoint: cfg.HubEndpoint,
	}
	cfg.FlatInstance = &FlatInstanceConfig{
		Identity: &brokeridentity.Identity{
			InstanceKey:     "cr-a",
			RuntimeBrokerID: "rb-flat-cr-a",
			RuntimeTarget:   api.RuntimeTargetDescriptor{ID: targetID, Type: brokeridentity.TargetTypeCloudRun, DisplayName: "Cloud Run"},
			ExecutionScope: brokeridentity.ExecutionScope{
				Type:     brokeridentity.TargetTypeCloudRun,
				CloudRun: &brokeridentity.CloudRunScope{ProjectNumber: "123456789012", Region: "us-central1", ProjectID: "example-project"},
			},
		},
		Instance: config.V1RuntimeBrokerInstanceConfig{
			Key:  "cr-a",
			Name: "cr-a",
			RuntimeTarget: &config.V1RuntimeTargetConfig{
				Type:                      config.RuntimeTargetTypeCloudRun,
				ProjectID:                 "example-project",
				Region:                    "us-central1",
				ServiceAccount:            strings.ToUpper(flatCRTestInstanceSA),
				BlockServiceAccount:       strings.ToUpper(flatCRTestBlockSA),
				AssignableServiceAccounts: []string{flatCRTestAssignSA},
				Subnetwork:                "agents-subnet",
				CredentialsFile:           "/etc/scion/cr-a.json",
			},
		},
		HubInProcess: true,
	}
	mgr := agent.NewManager(rt)
	srv := New(cfg, mgr, rt, config.RuntimeBuildRecord{})
	endAsyncLaunchesAtCleanup(t, srv)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	require.NoError(t, srv.ownershipSetupErr)
	return srv, fake, targetID
}

// TestFlatCloudRunIdentity_FakeInstancesLifecycle drives create -> stop ->
// start (same RunID to exercise clearExistingForIdentity delete-and-recreate,
// and new RunID) -> restart -> stop through Server.Handler() + AgentManager +
// CloudRunRuntime over a fake Cloud Run Instances API, for unset, marked
// block, and marked confined passthrough, with hub.cloudrun_native_identity
// both OFF and ON.
func TestFlatCloudRunIdentity_FakeInstancesLifecycle(t *testing.T) {
	modes := []struct {
		name      string
		createGCP map[string]any
		startEnv  map[string]string
	}{
		{
			name:     "unset without marker",
			startEnv: map[string]string{},
		},
		{
			name: "unset with sealed hub marker",
			startEnv: map[string]string{
				"SCION_METADATA_MODE_SOURCE": "hub",
				cloudRunNativeMarkerEnv:      "1",
			},
		},
		{
			name:      "marked block",
			createGCP: map[string]any{"metadata_mode": "block", "native_cloud_run": true},
			startEnv: map[string]string{
				"SCION_METADATA_MODE":        "block",
				"SCION_METADATA_MODE_SOURCE": "hub",
				cloudRunNativeMarkerEnv:      "1",
			},
		},
		{
			name:      "marked confined passthrough",
			createGCP: map[string]any{"metadata_mode": "passthrough", "require_local_runtime": true, "native_cloud_run": true},
			startEnv: map[string]string{
				"SCION_METADATA_MODE":                  "passthrough",
				"SCION_METADATA_MODE_SOURCE":           "hub",
				"SCION_METADATA_REQUIRE_LOCAL_RUNTIME": "true",
				cloudRunNativeMarkerEnv:                "1",
			},
		},
	}

	for _, gateOn := range []bool{false, true} {
		for _, tc := range modes {
			t.Run(fmt.Sprintf("gate=%t/%s", gateOn, tc.name), func(t *testing.T) {
				srv, fake, targetID := newFlatCloudRunLifecycleServer(t)
				const (
					agentName = "worker-i1"
					agentID   = "11111111-2222-4333-8444-555555555555"
					runCreate = "run-create-1"
					runStart  = "run-start-2"
					runRest   = "run-restart-3"
				)

				defaults := map[string]any{}
				if gateOn {
					defaults["experiments"] = []string{experiments.CloudRunNativeIdentity}
				}

				assertCreatedAsBlock := func(step string, inst *runpb.Instance) {
					t.Helper()
					assert.Equal(t, flatCRTestBlockSA, inst.GetServiceAccount(), "%s: must run as block_service_account, never service_account", step)
					idLabel := inst.GetLabels()["scion-identity"]
					assert.Len(t, idLabel, 16, "%s: scion-identity label must be a 16-hex hash", step)
					for k, v := range inst.GetLabels() {
						assert.NotContains(t, v, "@", "%s: label %s leaked email", step, k)
					}
					env := crInstanceEnvMap(inst)
					assert.Equal(t, "block", env["SCION_METADATA_MODE"], "%s: emulator mode must be block", step)
					assert.Equal(t, "localhost:18380", env["GCE_METADATA_HOST"], "%s: GCE_METADATA_HOST", step)
					assert.Equal(t, "localhost:18380", env["GCE_METADATA_ROOT"], "%s: GCE_METADATA_ROOT", step)
				}

				// 1. Create
				cfgMap := map[string]any{"template": "default"}
				if tc.createGCP != nil {
					cfgMap["gcpIdentity"] = tc.createGCP
				}
				if len(defaults) > 0 {
					cfgMap["hubAgentDefaults"] = defaults
				}
				createReq := map[string]any{
					"requestId":               "req-create-1",
					"name":                    agentName,
					"slug":                    agentName,
					"id":                      agentID,
					"runId":                   runCreate,
					"projectId":               flatCRTestProjectID,
					"projectSlug":             "flat-cr-proj",
					"workspaceMode":           "shared",
					"noAuth":                  true,
					"agentToken":              "test-agent-token",
					"hubEndpoint":             "https://hub.example.com",
					"expectedRuntimeTargetId": targetID,
					"config":                  cfgMap,
					"resolvedEnv":             maps.Clone(tc.startEnv),
				}
				rawCreate, err := json.Marshal(createReq)
				require.NoError(t, err)
				w := serveFlat(srv, http.MethodPost, "/api/v1/agents", string(rawCreate))
				require.Equal(t, http.StatusCreated, w.Code, "create failed: %s", w.Body.String())

				fake.mu.Lock()
				require.Len(t, fake.created, 1)
				firstInst := fake.created[0]
				assert.Empty(t, fake.started)
				fake.mu.Unlock()
				assertCreatedAsBlock("create", firstInst)

				// 2. Stop
				stopBody, _ := json.Marshal(map[string]any{"runId": runCreate})
				w = serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/stop?projectId="+flatCRTestProjectID, string(stopBody))
				require.Equal(t, http.StatusAccepted, w.Code, "stop after create failed: %s", w.Body.String())
				fake.mu.Lock()
				assert.Len(t, fake.stopped, 1)
				assert.Empty(t, fake.deleted, "stop leaves stopped instance in Cloud Run until recreated or deleted")
				fake.mu.Unlock()

				// 2b. Same-RunID HTTP start retry: proves clearExistingForIdentity
				// deletes the existing same-run instance and creates a replacement
				// without calling StartInstance.
				startEnv := maps.Clone(tc.startEnv)
				startEnv["SCION_AGENT_ID"] = agentID
				startEnv["SCION_AUTH_TOKEN"] = "test-agent-token"
				envClassifications := map[string]api.EnvKind{
					"SCION_AUTH_TOKEN": api.EnvKindSecretBootstrap,
				}
				sameRunStartPayload := map[string]any{
					"runId":                   runCreate,
					"noAuth":                  true,
					"hubEndpoint":             "https://hub.example.com",
					"expectedRuntimeTargetId": targetID,
					"resolvedEnv":             startEnv,
					"envClassifications":      envClassifications,
				}
				if len(defaults) > 0 {
					sameRunStartPayload["hubAgentDefaults"] = defaults
				}
				rawSameRunStart, _ := json.Marshal(sameRunStartPayload)
				w = serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/start?projectId="+flatCRTestProjectID, string(rawSameRunStart))
				require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, w.Code, "same-run start failed: %s", w.Body.String())
				fake.mu.Lock()
				require.Len(t, fake.created, 2, "clearExistingForIdentity must recreate same-run instance")
				assert.Len(t, fake.deleted, 1, "clearExistingForIdentity must delete existing same-run instance")
				assert.Empty(t, fake.started, "StartInstance must never be called")
				fake.mu.Unlock()
				assertCreatedAsBlock("same-run clearExistingForIdentity", fake.created[1])

				// Stop before new-RunID HTTP start
				w = serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/stop?projectId="+flatCRTestProjectID, string(stopBody))
				require.Equal(t, http.StatusAccepted, w.Code, "stop before start failed: %s", w.Body.String())

				// 3. Start (new RunID: deletes stopped instance and creates fresh instance; never calls StartInstance)
				startPayload := map[string]any{
					"runId":                   runStart,
					"noAuth":                  true,
					"hubEndpoint":             "https://hub.example.com",
					"expectedRuntimeTargetId": targetID,
					"resolvedEnv":             startEnv,
					"envClassifications":      envClassifications,
				}
				if len(defaults) > 0 {
					startPayload["hubAgentDefaults"] = defaults
				}
				rawStart, _ := json.Marshal(startPayload)
				w = serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/start?projectId="+flatCRTestProjectID, string(rawStart))
				require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, w.Code, "start failed: %s", w.Body.String())

				fake.mu.Lock()
				require.Len(t, fake.created, 3, "start must recreate the Cloud Run instance")
				secondInst := fake.created[2]
				assert.Len(t, fake.deleted, 2, "start must delete the prior stopped instance")
				assert.Empty(t, fake.started, "StartInstance must never be called")
				fake.mu.Unlock()
				assertCreatedAsBlock("start", secondInst)

				// 4. Restart (stops + deletes + creates fresh instance; never calls StartInstance)
				restartPayload := map[string]any{
					"runId":                   runRest,
					"noAuth":                  true,
					"hubEndpoint":             "https://hub.example.com",
					"expectedRuntimeTargetId": targetID,
					"resolvedEnv":             startEnv,
					"envClassifications":      envClassifications,
				}
				if len(defaults) > 0 {
					restartPayload["hubAgentDefaults"] = defaults
				}
				rawRestart, _ := json.Marshal(restartPayload)
				w = serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/restart?projectId="+flatCRTestProjectID, string(rawRestart))
				require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, w.Code, "restart failed: %s", w.Body.String())

				fake.mu.Lock()
				require.Len(t, fake.created, 4, "restart must recreate the Cloud Run instance")
				thirdInst := fake.created[3]
				assert.GreaterOrEqual(t, len(fake.deleted), 3, "restart must delete the prior instance")
				assert.Empty(t, fake.started, "StartInstance must never be called")
				fake.mu.Unlock()
				assertCreatedAsBlock("restart", thirdInst)

				// 5. Final Stop
				stopBody2, _ := json.Marshal(map[string]any{"runId": runRest})
				w = serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/stop?projectId="+flatCRTestProjectID, string(stopBody2))
				require.Equal(t, http.StatusAccepted, w.Code, "final stop failed: %s", w.Body.String())
			})
		}
	}
}

// TestFlatCloudRunIdentity_StartRestartLateRuntimeMismatchRefused verifies
// that start and restart on a flat Cloud Run instance run
// rejectCloudRunIdentityRuntimeChange even with hub.cloudrun_native_identity
// OFF: if the runtime changes between buildStartContext and the handler's
// authoritative resolution, the request is refused with 409 Conflict before
// stopping or starting the agent.
func TestFlatCloudRunIdentity_StartRestartLateRuntimeMismatchRefused(t *testing.T) {
	directions := []struct {
		name  string
		early string
		late  string
	}{
		{name: "cloudrun_to_docker", early: cloudRunRuntimeType, late: "docker"},
		{name: "docker_to_cloudrun", early: "docker", late: cloudRunRuntimeType},
	}
	for _, dir := range directions {
		for _, verb := range []string{"start", "restart"} {
			t.Run(dir.name+"/"+verb, func(t *testing.T) {
				srv, mgr, _ := newTestServerForLateCheckOrdering(t, "flat-late-agent", "url-id", cloudRunRuntimeType)
				target := config.V1RuntimeTargetConfig{
					Type:                config.RuntimeTargetTypeCloudRun,
					ServiceAccount:      flatCRTestInstanceSA,
					BlockServiceAccount: flatCRTestBlockSA,
				}
				srv.config.ContainerHubEndpoint = "https://hub.example.com"
				srv.config.FlatInstance = &FlatInstanceConfig{
					Identity: &brokeridentity.Identity{
						InstanceKey:     "cr-a",
						RuntimeBrokerID: "rb-flat-cr-a",
						RuntimeTarget:   api.RuntimeTargetDescriptor{ID: "target-flat-cr-a", Type: brokeridentity.TargetTypeCloudRun},
						ExecutionScope:  brokeridentity.ExecutionScope{Type: brokeridentity.TargetTypeCloudRun},
					},
					Instance:     config.V1RuntimeBrokerInstanceConfig{Key: "cr-a", Name: "cr-a", RuntimeTarget: &target},
					HubInProcess: true,
				}
				srv.flatCloudRunIdentity = newFlatCloudRunIdentityPolicy(srv.config.FlatInstance)

				defRT := srv.runtime.(*runtime.MockRuntime)
				defRT.NameFunc = func() string {
					if srv.cloudRunIdentityResolves.count() == 0 {
						return dir.early
					}
					return dir.late
				}
				defRT.RunFunc = func(context.Context, runtime.RunConfig) (string, error) {
					t.Error("agent must not run once late runtime mismatch is refused")
					return "", nil
				}

				body, err := json.Marshal(map[string]any{
					"expectedRuntimeTargetId": "target-flat-cr-a",
					"resolvedEnv":             map[string]string{},
				})
				require.NoError(t, err)
				w := serveFlat(srv, http.MethodPost, "/api/v1/agents/url-id/"+verb, string(body))
				assert.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
				assert.Contains(t, w.Body.String(), flatIdentityChangeRemedy)
				assert.NotContains(t, w.Body.String(), flatCRTestBlockSA)
				assert.Zero(t, mgr.stopCalls)
			})
		}
	}
}
