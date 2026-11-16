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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	flatCRTestAssignSA2  = "worker-assign-two@example-project.iam.gserviceaccount.com"
	flatCRTestUnlistedSA = "unlisted-worker@example-project.iam.gserviceaccount.com"
	flatCRTestProjectID  = "9a1b2c3d-4e5f-4a6b-8c7d-0e1f2a3b4c5d"
	flatCRTestGCPProject = "gcp-proj-assign"
)

func wantCRIdentityLabel(account string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(account)))
	return hex.EncodeToString(sum[:])[:16]
}

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
	instSA, ok := p.instanceAccount()
	require.True(t, ok)
	assert.Equal(t, flatCRTestInstanceSA, instSA)
	assert.True(t, p.accountAssignable(flatCRTestAssignSA))
	assert.True(t, p.accountAssignable("  WORKER-ASSIGN@example-project.iam.gserviceaccount.com  "))
	assert.False(t, p.accountAssignable(flatCRTestInstanceSA), "runtime_target.service_account is not implicitly allow-listed for assign")
	assert.False(t, p.accountAssignable(flatCRTestUnlistedSA))
	assert.False(t, p.accountAssignable(""))
	assert.Equal(t, map[string]struct{}{flatCRTestAssignSA: {}}, p.assignable)

	// Explicitly listing runtime_target.service_account in assignable_service_accounts allows it for assign.
	withInstanceAssignable := newFlatCloudRunIdentityPolicy(&FlatInstanceConfig{
		Instance: config.V1RuntimeBrokerInstanceConfig{
			Key: "cr-a",
			RuntimeTarget: &config.V1RuntimeTargetConfig{
				Type:                      config.RuntimeTargetTypeCloudRun,
				ServiceAccount:            flatCRTestInstanceSA,
				BlockServiceAccount:       flatCRTestBlockSA,
				AssignableServiceAccounts: []string{flatCRTestInstanceSA},
			},
		},
	})
	assert.True(t, withInstanceAssignable.accountAssignable(flatCRTestInstanceSA))

	// Empty / nil inputs fail closed.
	empty := newFlatCloudRunIdentityPolicy(nil)
	_, ok = empty.blockAccount()
	assert.False(t, ok)
	_, ok = empty.instanceAccount()
	assert.False(t, ok)
	assert.False(t, empty.accountAssignable(flatCRTestAssignSA))

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

	// Unvalidated server without service_account fails explicit passthrough closed,
	// naming server.broker.instances[<key>].runtime_target.service_account and no account.
	srvNoSA := newFlatCloudRunStartServer(t)
	srvNoSA.config.FlatInstance.Instance.RuntimeTarget.ServiceAccount = ""
	srvNoSA.flatCloudRunIdentity = newFlatCloudRunIdentityPolicy(srvNoSA.config.FlatInstance)
	scNoSA, errNoSA := srvNoSA.buildStartContext(context.Background(), startContextInputs{
		Name:        "agent-no-sa",
		Config:      &CreateAgentConfig{GCPIdentity: &GCPIdentityConfig{MetadataMode: "passthrough"}},
		HTTPRequest: httptest.NewRequest(http.MethodPost, "/api/v1/agents", nil),
		Operation:   opCreate,
	})
	require.Nil(t, scNoSA)
	sceNoSA := requireStartContextError(t, errNoSA)
	assert.Equal(t, http.StatusBadRequest, sceNoSA.Status)
	assert.Contains(t, sceNoSA.Message, "server.broker.instances[cr-a].runtime_target.service_account")
	assert.NotContains(t, sceNoSA.Message, "@")
}

// crLifecycleInstances is a stateful fake cloudrun.InstancesAPI supporting
// CreateInstance, GetInstance, ListInstances, StopInstance, DeleteInstance,
// and tracking StartInstance (which must remain 0 under native identity).
type crLifecycleInstances struct {
	mu           sync.Mutex
	gen          int64
	live         map[string]*runpb.Instance
	hideFromList bool
	created      []*runpb.Instance
	started      []string
	stopped      []string
	deleted      []string
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
	if c.hideFromList {
		return &crIter{}
	}
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

// TestFlatCloudRunIdentity_AssignAndPassthroughLifecycle drives create -> stop ->
// same-run start (clearExistingForIdentity) -> stop -> start (new RunID) ->
// restart -> stop through Server.Handler() + AgentManager + CloudRunRuntime over
// fake Cloud Run Instances, for allow-listed assign (including case-insensitive
// normalization and listing runtime_target.service_account in
// assignable_service_accounts) and explicit passthrough (both without and with
// the native marker), with hub.cloudrun_native_identity both OFF and ON.
func TestFlatCloudRunIdentity_AssignAndPassthroughLifecycle(t *testing.T) {
	modes := []struct {
		name              string
		assignable        []string
		createGCP         map[string]any
		startEnv          map[string]string
		wantSA            string
		wantInformational bool
	}{
		{
			name:       "assign with allow-listed SA (normalized case)",
			assignable: []string{flatCRTestAssignSA},
			createGCP: map[string]any{
				"metadata_mode":    "assign",
				"sa_email":         "  " + strings.ToUpper(flatCRTestAssignSA) + "  ",
				"project_id":       flatCRTestGCPProject,
				"native_cloud_run": true,
			},
			startEnv: map[string]string{
				"SCION_METADATA_MODE":        "assign",
				"SCION_METADATA_MODE_SOURCE": "hub",
				"SCION_METADATA_SA_EMAIL":    strings.ToUpper(flatCRTestAssignSA),
				"SCION_METADATA_PROJECT_ID":  flatCRTestGCPProject,
				cloudRunNativeMarkerEnv:      "1",
			},
			wantSA:            flatCRTestAssignSA,
			wantInformational: true,
		},
		{
			name:       "assign with runtime_target.service_account explicitly allow-listed",
			assignable: []string{flatCRTestAssignSA, flatCRTestInstanceSA},
			createGCP: map[string]any{
				"metadata_mode":    "assign",
				"sa_email":         flatCRTestInstanceSA,
				"project_id":       flatCRTestGCPProject,
				"native_cloud_run": true,
			},
			startEnv: map[string]string{
				"SCION_METADATA_MODE":        "assign",
				"SCION_METADATA_MODE_SOURCE": "hub",
				"SCION_METADATA_SA_EMAIL":    flatCRTestInstanceSA,
				"SCION_METADATA_PROJECT_ID":  flatCRTestGCPProject,
				cloudRunNativeMarkerEnv:      "1",
			},
			wantSA:            flatCRTestInstanceSA,
			wantInformational: true,
		},
		{
			name:       "explicit passthrough without native marker",
			assignable: []string{flatCRTestAssignSA},
			createGCP:  map[string]any{"metadata_mode": "passthrough"},
			startEnv: map[string]string{
				"SCION_METADATA_MODE":        "passthrough",
				"SCION_METADATA_MODE_SOURCE": "hub",
			},
			wantSA:            flatCRTestInstanceSA,
			wantInformational: false,
		},
		{
			name:       "explicit passthrough with native marker",
			assignable: []string{flatCRTestAssignSA},
			createGCP:  map[string]any{"metadata_mode": "passthrough", "native_cloud_run": true},
			startEnv: map[string]string{
				"SCION_METADATA_MODE":        "passthrough",
				"SCION_METADATA_MODE_SOURCE": "hub",
				cloudRunNativeMarkerEnv:      "1",
			},
			wantSA:            flatCRTestInstanceSA,
			wantInformational: false,
		},
	}

	for _, gateOn := range []bool{false, true} {
		for _, tc := range modes {
			t.Run(fmt.Sprintf("gate=%t/%s", gateOn, tc.name), func(t *testing.T) {
				srv, fake, targetID := newFlatCloudRunLifecycleServer(t)
				srv.config.FlatInstance.Instance.RuntimeTarget.AssignableServiceAccounts = tc.assignable
				srv.flatCloudRunIdentity = newFlatCloudRunIdentityPolicy(srv.config.FlatInstance)

				const (
					agentName = "worker-i2"
					agentID   = "22222222-3333-4444-8555-666666666666"
					runCreate = "run-i2-create-1"
					runStart  = "run-i2-start-2"
					runRest   = "run-i2-restart-3"
				)

				defaults := map[string]any{}
				if gateOn {
					defaults["experiments"] = []string{experiments.CloudRunNativeIdentity}
				}

				assertInstanceIdentity := func(step string, inst *runpb.Instance) {
					t.Helper()
					assert.Equal(t, tc.wantSA, inst.GetServiceAccount(), "%s: instance ServiceAccount", step)
					assert.Equal(t, wantCRIdentityLabel(tc.wantSA), inst.GetLabels()["scion-identity"], "%s: scion-identity hash", step)
					for k, v := range inst.GetLabels() {
						assert.NotContains(t, v, "@", "%s: label %s leaked email", step, k)
					}
					env := crInstanceEnvMap(inst)
					assert.Equal(t, "passthrough", env["SCION_METADATA_MODE"], "%s: real Cloud Run metadata server mode", step)
					for _, key := range []string{"GCE_METADATA_HOST", "GCE_METADATA_ROOT", "SCION_METADATA_PORT"} {
						_, present := env[key]
						assert.False(t, present, "%s: %s must not redirect to emulator", step, key)
					}
					if tc.wantInformational {
						assert.Equal(t, tc.wantSA, env["SCION_METADATA_SA_EMAIL"], "%s: informational SA email", step)
						assert.Equal(t, flatCRTestGCPProject, env["SCION_METADATA_PROJECT_ID"], "%s: informational project ID", step)
					} else {
						_, hasSA := env["SCION_METADATA_SA_EMAIL"]
						assert.False(t, hasSA, "%s: explicit passthrough must not expose runtime_target.service_account in env", step)
					}
				}

				// 1. Create
				cfgMap := map[string]any{
					"template":    "default",
					"gcpIdentity": tc.createGCP,
				}
				if len(defaults) > 0 {
					cfgMap["hubAgentDefaults"] = defaults
				}
				// On create, Hub sends only SCION_METADATA_MODE + SOURCE (+ native marker), never SA_EMAIL in env.
				createEnv := map[string]string{
					"SCION_METADATA_MODE":        tc.startEnv["SCION_METADATA_MODE"],
					"SCION_METADATA_MODE_SOURCE": "hub",
				}
				if m, ok := tc.startEnv[cloudRunNativeMarkerEnv]; ok {
					createEnv[cloudRunNativeMarkerEnv] = m
				}
				rawCreate, err := json.Marshal(map[string]any{
					"requestId":               "req-i2-create-1",
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
					"resolvedEnv":             createEnv,
				})
				require.NoError(t, err)
				w := serveFlat(srv, http.MethodPost, "/api/v1/agents", string(rawCreate))
				require.Equal(t, http.StatusCreated, w.Code, "create failed: %s", w.Body.String())

				fake.mu.Lock()
				require.Len(t, fake.created, 1)
				assert.Empty(t, fake.started)
				firstInst := fake.created[0]
				fake.mu.Unlock()
				assertInstanceIdentity("create", firstInst)

				// 2. Stop + same-RunID start (clearExistingForIdentity)
				stopCreate, _ := json.Marshal(map[string]any{"runId": runCreate})
				w = serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/stop?projectId="+flatCRTestProjectID, string(stopCreate))
				require.Equal(t, http.StatusAccepted, w.Code)

				startEnv := maps.Clone(tc.startEnv)
				startEnv["SCION_AGENT_ID"] = agentID
				startEnv["SCION_AUTH_TOKEN"] = "test-agent-token"
				envClassifications := map[string]api.EnvKind{
					"SCION_AUTH_TOKEN": api.EnvKindSecretBootstrap,
				}
				sameRunPayload := map[string]any{
					"runId":                   runCreate,
					"noAuth":                  true,
					"hubEndpoint":             "https://hub.example.com",
					"expectedRuntimeTargetId": targetID,
					"resolvedEnv":             startEnv,
					"envClassifications":      envClassifications,
				}
				if len(defaults) > 0 {
					sameRunPayload["hubAgentDefaults"] = defaults
				}
				rawSameRun, _ := json.Marshal(sameRunPayload)
				w = serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/start?projectId="+flatCRTestProjectID, string(rawSameRun))
				require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, w.Code, "same-run start failed: %s", w.Body.String())
				fake.mu.Lock()
				require.Len(t, fake.created, 2)
				assert.Len(t, fake.deleted, 1)
				assert.Empty(t, fake.started)
				sameRunInst := fake.created[1]
				fake.mu.Unlock()
				assertInstanceIdentity("same-run start", sameRunInst)

				// 3. Stop + new-RunID start
				w = serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/stop?projectId="+flatCRTestProjectID, string(stopCreate))
				require.Equal(t, http.StatusAccepted, w.Code)

				startPayload := maps.Clone(sameRunPayload)
				startPayload["runId"] = runStart
				rawStart, _ := json.Marshal(startPayload)
				w = serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/start?projectId="+flatCRTestProjectID, string(rawStart))
				require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, w.Code, "start failed: %s", w.Body.String())
				fake.mu.Lock()
				require.Len(t, fake.created, 3)
				assert.Empty(t, fake.started)
				secondInst := fake.created[2]
				fake.mu.Unlock()
				assertInstanceIdentity("start", secondInst)

				// 4. Restart
				restartPayload := maps.Clone(sameRunPayload)
				restartPayload["runId"] = runRest
				rawRestart, _ := json.Marshal(restartPayload)
				w = serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/restart?projectId="+flatCRTestProjectID, string(rawRestart))
				require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, w.Code, "restart failed: %s", w.Body.String())
				fake.mu.Lock()
				require.Len(t, fake.created, 4)
				assert.Empty(t, fake.started)
				thirdInst := fake.created[3]
				fake.mu.Unlock()
				assertInstanceIdentity("restart", thirdInst)

				// 5. Final Stop
				stopRest, _ := json.Marshal(map[string]any{"runId": runRest})
				w = serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/stop?projectId="+flatCRTestProjectID, string(stopRest))
				require.Equal(t, http.StatusAccepted, w.Code)
			})
		}
	}
}

// TestFlatCloudRunIdentity_FakeInstancesRefusalsNoSideEffects exercises HTTP
// POST /api/v1/agents against a real flat Cloud Run broker backed by
// crLifecycleInstances and verifies that unlisted assign SA, empty allow-list
// assign, runtime_target.service_account when not in assignable_service_accounts,
// unmarked assign, and assign + SCION_INIT_FIREWALL all return HTTP 400 with
// zero ownership records, zero project files, zero CreateInstance calls, and no
// service account email in the response body or logs.
func TestFlatCloudRunIdentity_FakeInstancesRefusalsNoSideEffects(t *testing.T) {
	cases := []struct {
		name       string
		assignable []string
		createGCP  map[string]any
		cfgEnv     []string
		wantCode   string
		wantSubstr string
	}{
		{
			name:       "unlisted assign SA",
			assignable: []string{flatCRTestAssignSA},
			createGCP: map[string]any{
				"metadata_mode":    "assign",
				"sa_email":         flatCRTestUnlistedSA,
				"project_id":       flatCRTestGCPProject,
				"native_cloud_run": true,
			},
			wantCode:   ErrCodeValidationError,
			wantSubstr: "server.broker.instances[cr-a].runtime_target.assignable_service_accounts",
		},
		{
			name:       "empty allow-list assign",
			assignable: nil,
			createGCP: map[string]any{
				"metadata_mode":    "assign",
				"sa_email":         flatCRTestAssignSA,
				"project_id":       flatCRTestGCPProject,
				"native_cloud_run": true,
			},
			wantCode:   ErrCodeValidationError,
			wantSubstr: "server.broker.instances[cr-a].runtime_target.assignable_service_accounts",
		},
		{
			name:       "runtime_target.service_account not implicitly allow-listed for assign",
			assignable: []string{flatCRTestAssignSA},
			createGCP: map[string]any{
				"metadata_mode":    "assign",
				"sa_email":         flatCRTestInstanceSA,
				"project_id":       flatCRTestGCPProject,
				"native_cloud_run": true,
			},
			wantCode:   ErrCodeValidationError,
			wantSubstr: "server.broker.instances[cr-a].runtime_target.assignable_service_accounts",
		},
		{
			name:       "assign without native marker (skew)",
			assignable: []string{flatCRTestAssignSA},
			createGCP: map[string]any{
				"metadata_mode": "assign",
				"sa_email":      flatCRTestAssignSA,
				"project_id":    flatCRTestGCPProject,
			},
			wantCode:   ErrCodeValidationError,
			wantSubstr: "did not mark this dispatch for native Cloud Run identity",
		},
		{
			name:       "assign with SCION_INIT_FIREWALL",
			assignable: []string{flatCRTestAssignSA},
			createGCP: map[string]any{
				"metadata_mode":    "assign",
				"sa_email":         flatCRTestAssignSA,
				"project_id":       flatCRTestGCPProject,
				"native_cloud_run": true,
			},
			cfgEnv:     []string{cloudRunInitFirewallEnv + "=true"},
			wantCode:   ErrCodeValidationError,
			wantSubstr: cloudRunInitFirewallRefusal,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, fake, targetID := newFlatCloudRunLifecycleServer(t)
			srv.config.FlatInstance.Instance.RuntimeTarget.AssignableServiceAccounts = tc.assignable
			srv.flatCloudRunIdentity = newFlatCloudRunIdentityPolicy(srv.config.FlatInstance)
			logs := probeFlatTokenStep(srv)

			home, err := os.UserHomeDir()
			require.NoError(t, err)
			globalDir, err := config.GetGlobalDir()
			require.NoError(t, err)
			projPath := config.BrokerProjectPath(globalDir, "", "flat-cr-refused-proj")
			recordsBefore, err := srv.ownership.List()
			require.NoError(t, err)
			homeBefore := e10TreeSnapshot(t, home)

			cfgMap := map[string]any{
				"template":    "default",
				"gcpIdentity": tc.createGCP,
			}
			if len(tc.cfgEnv) > 0 {
				cfgMap["env"] = tc.cfgEnv
			}
			rawCreate, err := json.Marshal(map[string]any{
				"requestId":               "req-refused-1",
				"name":                    "worker-refused",
				"slug":                    "worker-refused",
				"id":                      "33333333-4444-4555-8666-777777777777",
				"runId":                   "run-refused-1",
				"projectId":               flatCRTestProjectID,
				"projectSlug":             "flat-cr-refused-proj",
				"workspaceMode":           "shared",
				"noAuth":                  true,
				"agentToken":              "test-agent-token",
				"hubEndpoint":             "https://hub.example.com",
				"expectedRuntimeTargetId": targetID,
				"config":                  cfgMap,
			})
			require.NoError(t, err)
			w := serveFlat(srv, http.MethodPost, "/api/v1/agents", string(rawCreate))
			require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())

			var errResp struct {
				Error struct {
					Code    string         `json:"code"`
					Message string         `json:"message"`
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &errResp))
			assert.Equal(t, tc.wantCode, errResp.Error.Code)
			assert.Contains(t, errResp.Error.Message, tc.wantSubstr)
			assert.NotContains(t, w.Body.String(), "@", "response must not leak any service account email")
			assert.NotContains(t, logs.String(), "@", "logs must not leak any service account email")

			recordsAfter, err := srv.ownership.List()
			require.NoError(t, err)
			assert.True(t, reflect.DeepEqual(recordsBefore, recordsAfter), "ownership records changed on refusal")
			assert.True(t, maps.Equal(homeBefore, e10TreeSnapshot(t, home)), "project files written on refusal")
			_, statErr := os.Lstat(projPath)
			assert.True(t, os.IsNotExist(statErr), "fresh project path created on refusal")

			fake.mu.Lock()
			assert.Empty(t, fake.created, "CreateInstance must not be called on refusal")
			assert.Empty(t, fake.started)
			fake.mu.Unlock()
		})
	}
}

// TestFlatCloudRunIdentity_IdentityChangeRecreatesInstance proves that starting
// or restarting an existing Cloud Run Instance after its resolved GCP identity
// changes deletes the old instance and creates a replacement with the new
// ServiceAccount and scion-identity hash label (never calling StartInstance),
// across:
//  1. block_service_account -> assign SA1 -> assign SA2 -> passthrough service_account -> block_service_account
//     (exercising both same-RunID clearExistingForIdentity and new-RunID start/restart);
//  2. a pre-Part-B instance carrying the run's scion-run-id label and no
//     scion-identity label -> replaced with block_service_account and scion-identity label;
//  3. an unattributed instance with no scion-run-id label -> refused with 409 Conflict,
//     neither started nor deleted.
func TestFlatCloudRunIdentity_IdentityChangeRecreatesInstance(t *testing.T) {
	t.Run("transitions_across_block_assign_and_passthrough", func(t *testing.T) {
		srv, fake, targetID := newFlatCloudRunLifecycleServer(t)
		srv.config.FlatInstance.Instance.RuntimeTarget.AssignableServiceAccounts = []string{flatCRTestAssignSA, flatCRTestAssignSA2}
		srv.flatCloudRunIdentity = newFlatCloudRunIdentityPolicy(srv.config.FlatInstance)

		const (
			agentName = "worker-recreate"
			agentID   = "44444444-5555-4666-8777-888888888888"
		)
		envClassifications := map[string]api.EnvKind{
			"SCION_AUTH_TOKEN": api.EnvKindSecretBootstrap,
		}

		// Step 1: Create under block -> runs as flatCRTestBlockSA
		rawCreate, err := json.Marshal(map[string]any{
			"requestId":               "req-rec-1",
			"name":                    agentName,
			"slug":                    agentName,
			"id":                      agentID,
			"runId":                   "run-step-1",
			"projectId":               flatCRTestProjectID,
			"projectSlug":             "flat-cr-recreate-proj",
			"workspaceMode":           "shared",
			"noAuth":                  true,
			"agentToken":              "test-agent-token",
			"hubEndpoint":             "https://hub.example.com",
			"expectedRuntimeTargetId": targetID,
			"config": map[string]any{
				"template":    "default",
				"gcpIdentity": map[string]any{"metadata_mode": "block", "native_cloud_run": true},
			},
			"resolvedEnv": map[string]string{
				"SCION_METADATA_MODE":        "block",
				"SCION_METADATA_MODE_SOURCE": "hub",
				cloudRunNativeMarkerEnv:      "1",
			},
		})
		require.NoError(t, err)
		w := serveFlat(srv, http.MethodPost, "/api/v1/agents", string(rawCreate))
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

		fake.mu.Lock()
		require.Len(t, fake.created, 1)
		assert.Equal(t, flatCRTestBlockSA, fake.created[0].GetServiceAccount())
		assert.Equal(t, wantCRIdentityLabel(flatCRTestBlockSA), fake.created[0].GetLabels()["scion-identity"])
		fake.mu.Unlock()

		transitions := []struct {
			name         string
			verb         string
			stopFirst    bool
			hideFromList bool
			prevRunID    string
			runID        string
			env          map[string]string
			wantSA       string
		}{
			{
				name:         "same-run clearExistingForIdentity: block -> assign SA1",
				verb:         "start",
				hideFromList: true,
				prevRunID:    "run-step-1",
				runID:        "run-step-1",
				env: map[string]string{
					"SCION_METADATA_MODE":        "assign",
					"SCION_METADATA_MODE_SOURCE": "hub",
					"SCION_METADATA_SA_EMAIL":    flatCRTestAssignSA,
					"SCION_METADATA_PROJECT_ID":  flatCRTestGCPProject,
					cloudRunNativeMarkerEnv:      "1",
				},
				wantSA: flatCRTestAssignSA,
			},
			{
				name:      "restart: assign SA1 -> assign SA2",
				verb:      "restart",
				prevRunID: "run-step-1",
				runID:     "run-step-2",
				env: map[string]string{
					"SCION_METADATA_MODE":        "assign",
					"SCION_METADATA_MODE_SOURCE": "hub",
					"SCION_METADATA_SA_EMAIL":    flatCRTestAssignSA2,
					"SCION_METADATA_PROJECT_ID":  flatCRTestGCPProject,
					cloudRunNativeMarkerEnv:      "1",
				},
				wantSA: flatCRTestAssignSA2,
			},
			{
				name:      "stop + start: assign SA2 -> explicit passthrough (no marker)",
				verb:      "start",
				stopFirst: true,
				prevRunID: "run-step-2",
				runID:     "run-step-3",
				env: map[string]string{
					"SCION_METADATA_MODE":        "passthrough",
					"SCION_METADATA_MODE_SOURCE": "hub",
				},
				wantSA: flatCRTestInstanceSA,
			},
			{
				name:      "restart: explicit passthrough -> block_service_account",
				verb:      "restart",
				prevRunID: "run-step-3",
				runID:     "run-step-4",
				env: map[string]string{
					"SCION_METADATA_MODE":        "block",
					"SCION_METADATA_MODE_SOURCE": "hub",
					cloudRunNativeMarkerEnv:      "1",
				},
				wantSA: flatCRTestBlockSA,
			},
		}

		for i, tr := range transitions {
			if tr.stopFirst {
				stopBody, _ := json.Marshal(map[string]any{"runId": tr.prevRunID})
				sw := serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/stop?projectId="+flatCRTestProjectID, string(stopBody))
				require.Equal(t, http.StatusAccepted, sw.Code, "%s stop failed: %s", tr.name, sw.Body.String())
			}
			fake.mu.Lock()
			fake.hideFromList = tr.hideFromList
			fake.mu.Unlock()

			env := maps.Clone(tr.env)
			env["SCION_AGENT_ID"] = agentID
			env["SCION_AUTH_TOKEN"] = "test-agent-token"
			raw, err := json.Marshal(map[string]any{
				"runId":                   tr.runID,
				"projectSlug":             "flat-cr-recreate-proj",
				"sharedWorkspace":         true,
				"noAuth":                  true,
				"hubEndpoint":             "https://hub.example.com",
				"expectedRuntimeTargetId": targetID,
				"resolvedEnv":             env,
				"envClassifications":      envClassifications,
			})
			require.NoError(t, err)
			w := serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/"+tr.verb+"?projectId="+flatCRTestProjectID, string(raw))
			fake.mu.Lock()
			fake.hideFromList = false
			fake.mu.Unlock()
			require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, w.Code, "%s failed: %s", tr.name, w.Body.String())

			fake.mu.Lock()
			require.Len(t, fake.created, i+2, "%s: must create a replacement instance", tr.name)
			assert.GreaterOrEqual(t, len(fake.deleted), i+1, "%s: must delete the old instance first", tr.name)
			assert.Empty(t, fake.started, "%s: StartInstance must never be called", tr.name)
			latest := fake.created[len(fake.created)-1]
			fake.mu.Unlock()

			assert.Equal(t, tr.wantSA, latest.GetServiceAccount(), "%s: ServiceAccount", tr.name)
			assert.Equal(t, wantCRIdentityLabel(tr.wantSA), latest.GetLabels()["scion-identity"], "%s: scion-identity label", tr.name)
		}
	})

	t.Run("pre_part_b_instance_without_scion_identity_label_recreated_and_unattributed_refused", func(t *testing.T) {
		srv, fake, targetID := newFlatCloudRunLifecycleServer(t)
		const (
			agentName = "worker-legacy-id"
			agentID   = "55555555-6666-4777-8888-999999999999"
			runID     = "run-legacy-1"
		)
		rawCreate, err := json.Marshal(map[string]any{
			"requestId":               "req-leg-1",
			"name":                    agentName,
			"slug":                    agentName,
			"id":                      agentID,
			"runId":                   runID,
			"projectId":               flatCRTestProjectID,
			"projectSlug":             "flat-cr-legacy-proj",
			"workspaceMode":           "shared",
			"noAuth":                  true,
			"agentToken":              "test-agent-token",
			"hubEndpoint":             "https://hub.example.com",
			"expectedRuntimeTargetId": targetID,
			"config":                  map[string]any{"template": "default"},
		})
		require.NoError(t, err)
		w := serveFlat(srv, http.MethodPost, "/api/v1/agents", string(rawCreate))
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

		// Simulate a pre-Part-B instance holding the deterministic instance ID
		// directly at CloudRunRuntime.Run (hideFromList skips pre-clean so
		// clearExistingForIdentity runs directly): strip scion-identity and set
		// ServiceAccount=instance_sa.
		fake.mu.Lock()
		require.Len(t, fake.live, 1)
		var instFullName string
		for k, liveInst := range fake.live {
			instFullName = k
			delete(liveInst.Labels, "scion-identity")
			liveInst.ServiceAccount = flatCRTestInstanceSA
		}
		fake.hideFromList = true
		fake.mu.Unlock()

		rawStart, _ := json.Marshal(map[string]any{
			"runId":                   runID,
			"projectSlug":             "flat-cr-legacy-proj",
			"sharedWorkspace":         true,
			"noAuth":                  true,
			"hubEndpoint":             "https://hub.example.com",
			"expectedRuntimeTargetId": targetID,
			"resolvedEnv": map[string]string{
				"SCION_AGENT_ID":   agentID,
				"SCION_AUTH_TOKEN": "test-agent-token",
			},
			"envClassifications": map[string]api.EnvKind{
				"SCION_AUTH_TOKEN": api.EnvKindSecretBootstrap,
			},
		})
		w = serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/start?projectId="+flatCRTestProjectID, string(rawStart))
		require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, w.Code, w.Body.String())

		fake.mu.Lock()
		require.Len(t, fake.created, 2, "pre-Part-B instance without scion-identity label must be deleted and recreated")
		assert.Len(t, fake.deleted, 1)
		assert.Empty(t, fake.started)
		recreated := fake.created[1]
		// Now simulate a truly unattributed legacy instance with NO scion_run_id label at all.
		delete(fake.live[instFullName].Labels, "scion_run_id")
		delete(fake.live[instFullName].Labels, "scion-identity")
		fake.mu.Unlock()

		assert.Equal(t, flatCRTestBlockSA, recreated.GetServiceAccount())
		assert.Equal(t, wantCRIdentityLabel(flatCRTestBlockSA), recreated.GetLabels()["scion-identity"])

		w = serveFlat(srv, http.MethodPost, "/api/v1/agents/"+agentName+"/start?projectId="+flatCRTestProjectID, string(rawStart))
		assert.Equal(t, http.StatusConflict, w.Code, "unattributed instance without scion_run_id must fail closed with 409: %s", w.Body.String())
		fake.mu.Lock()
		assert.Len(t, fake.deleted, 1, "unattributed instance without scion_run_id must not be deleted")
		assert.Empty(t, fake.started)
		fake.mu.Unlock()
	})
}

// TestFlatCloudRunIdentity_StartRestartLateRuntimeMismatchRefused verifies
// that start and restart on a flat Cloud Run instance run
// rejectCloudRunIdentityRuntimeChange across unset, assign, and explicit
// passthrough even with hub.cloudrun_native_identity OFF: if the runtime or
// flat selection changes between buildStartContext and the handler's
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
	identities := []struct {
		name string
		env  map[string]string
	}{
		{name: "unset", env: map[string]string{}},
		{
			name: "assign",
			env: map[string]string{
				"SCION_METADATA_MODE":        "assign",
				"SCION_METADATA_MODE_SOURCE": "hub",
				"SCION_METADATA_SA_EMAIL":    flatCRTestAssignSA,
				"SCION_METADATA_PROJECT_ID":  flatCRTestGCPProject,
				cloudRunNativeMarkerEnv:      "1",
			},
		},
		{
			name: "explicit_passthrough",
			env: map[string]string{
				"SCION_METADATA_MODE":        "passthrough",
				"SCION_METADATA_MODE_SOURCE": "hub",
			},
		},
	}
	for _, dir := range directions {
		for _, idCase := range identities {
			for _, verb := range []string{"start", "restart"} {
				t.Run(dir.name+"/"+idCase.name+"/"+verb, func(t *testing.T) {
					srv, mgr, _ := newTestServerForLateCheckOrdering(t, "flat-late-agent", "url-id", cloudRunRuntimeType)
					target := config.V1RuntimeTargetConfig{
						Type:                      config.RuntimeTargetTypeCloudRun,
						ServiceAccount:            flatCRTestInstanceSA,
						BlockServiceAccount:       flatCRTestBlockSA,
						AssignableServiceAccounts: []string{flatCRTestAssignSA},
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
						"resolvedEnv":             maps.Clone(idCase.env),
					})
					require.NoError(t, err)
					w := serveFlat(srv, http.MethodPost, "/api/v1/agents/url-id/"+verb, string(body))
					assert.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
					assert.Contains(t, w.Body.String(), flatIdentityChangeRemedy)
					assert.NotContains(t, w.Body.String(), "@")
					assert.Zero(t, mgr.stopCalls)
				})
			}
		}
	}
}
