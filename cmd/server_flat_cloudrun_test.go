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

package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/googleapis/gax-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// flatCRInstances is a read-only cloudrun.InstancesAPI double for the
// ownership preflight: it lists and gets a fixed set of instances; every
// mutating call fails.
type flatCRInstances struct{ instances []*runpb.Instance }

type flatCRIter struct{ instances []*runpb.Instance }

func (it *flatCRIter) Next() (*runpb.Instance, error) {
	if len(it.instances) == 0 {
		return nil, iterator.Done
	}
	inst := it.instances[0]
	it.instances = it.instances[1:]
	return inst, nil
}

func (c *flatCRInstances) GetInstance(_ context.Context, req *runpb.GetInstanceRequest, _ ...gax.CallOption) (*runpb.Instance, error) {
	for _, inst := range c.instances {
		if inst.GetName() == req.GetName() {
			return inst, nil
		}
	}
	return nil, status.Error(codes.NotFound, "instance does not exist")
}

func (c *flatCRInstances) CreateInstance(context.Context, *runpb.CreateInstanceRequest, ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	return nil, errors.New("unexpected CreateInstance")
}

func (c *flatCRInstances) StartInstance(context.Context, *runpb.StartInstanceRequest, ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	return nil, errors.New("unexpected StartInstance")
}

func (c *flatCRInstances) StopInstance(context.Context, *runpb.StopInstanceRequest, ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	return nil, errors.New("unexpected StopInstance")
}

func (c *flatCRInstances) DeleteInstance(context.Context, *runpb.DeleteInstanceRequest, ...gax.CallOption) (cloudrun.InstanceOperation, error) {
	return nil, errors.New("unexpected DeleteInstance")
}

func (c *flatCRInstances) ListInstances(context.Context, *runpb.ListInstancesRequest, ...gax.CallOption) cloudrun.InstanceIterator {
	return &flatCRIter{instances: append([]*runpb.Instance(nil), c.instances...)}
}

func (c *flatCRInstances) Close() error { return nil }

const (
	flatCRProject = "example-project"
	flatCRScionPr = "5a4b3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d"
)

// flatCRRuntime is a real CloudRunRuntime for region over fake.
func flatCRRuntime(t *testing.T, region string, fake *flatCRInstances) *runtime.CloudRunRuntime {
	t.Helper()
	rt, err := runtime.NewCloudRunRuntimeForTest(&config.CloudRunConfig{ProjectID: flatCRProject, Location: region},
		func(context.Context) (cloudrun.InstancesAPI, error) { return fake, nil })
	require.NoError(t, err)
	return rt
}

// flatCRInstance is an agent instance in region as CloudRunRuntime.Run
// stores it: GCP-sanitized label keys. owner "" leaves the owner label off
// (a legacy agent).
func flatCRInstance(region, instanceID, owner, agentID, slug, runID string) *runpb.Instance {
	labels := map[string]string{
		"scion_agent":      "true",
		"scion_project_id": flatCRScionPr,
		"agent_id":         agentID,
		"scion_name":       slug,
		"scion_run_id":     runID,
	}
	if owner != "" {
		labels["scion_runtime_broker_id"] = owner
	}
	return &runpb.Instance{Name: "projects/" + flatCRProject + "/locations/" + region + "/instances/" + instanceID, Labels: labels}
}

// flatCRTarget is a valid cloudrun runtime_target in region.
func flatCRTarget(region string) *config.V1RuntimeTargetConfig {
	return &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeCloudRun, ProjectID: flatCRProject, Region: region,
		ServiceAccount: "agents@example-project.iam.gserviceaccount.com", Subnetwork: "agents-subnet",
		CredentialsFile: "/etc/scion/cr-" + region + ".json"}
}

// TestFlatOwnershipPreflight_CloudRunSanitizedLabels: on a Cloud Run scope,
// whose labels are stored under GCP-sanitized keys, this instance's own
// instance reconstructs its record with the launch's handle, another
// instance's object is ignored, and an object without an owner label
// refuses the instance, named by its project and slug.
func TestFlatOwnershipPreflight_CloudRunSanitizedLabels(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	const (
		mine   = "6c0f9a3e-8d1b-4e2f-a5c7-3b9d0e1f2a4c"
		theirs = "7d1e0b4f-9e2c-4f30-b6d8-4c0e1f2a3b5d"
	)
	candidate := func(fake *flatCRInstances) brokerhost.Candidate {
		return brokerhost.Candidate{
			Instance: config.V1RuntimeBrokerInstanceConfig{Key: "cr-a", Name: "a", RuntimeTarget: flatCRTarget("us-central1")},
			Identity: &brokeridentity.Identity{InstanceKey: "cr-a", RuntimeBrokerID: mine},
			Runtime:  flatCRRuntime(t, "us-central1", fake),
		}
	}
	owned := flatCRInstance("us-central1", "worker-0123456789", mine, "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a", "worker", "0f1e2d3c-4b5a-4968-8776-655443322110")
	other := flatCRInstance("us-central1", "helper-0123456789", theirs, "1a2b3c4d-5e6f-4071-8293-a4b5c6d7e8f9", "helper", "21436587-a9cb-4fed-8102-132435465768")
	require.NoError(t, flatOwnershipPreflight(ctx, candidate(&flatCRInstances{instances: []*runpb.Instance{owned, other}})))

	dir, err := runtimebroker.DefaultStateDir(mine)
	require.NoError(t, err)
	store := runtimebroker.NewOwnershipStore(dir, mine)
	rec, ok, err := store.Get(flatCRScionPr, "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a")
	require.NoError(t, err)
	require.True(t, ok, "the owned instance's record is reconstructed from its sanitized labels")
	run := rec.Run("0f1e2d3c-4b5a-4968-8776-655443322110")
	require.NotNil(t, run)
	require.Len(t, run.Resources, 1)
	assert.Equal(t, api.ResourceKindCloudRunInstance, run.Resources[0].Kind)
	assert.Equal(t, "projects/"+flatCRProject+"/locations/us-central1", run.Resources[0].Namespace)
	assert.Equal(t, "worker-0123456789", run.Resources[0].Name)
	assert.Equal(t, runtimebroker.OwnedResourceRecorded, run.Resources[0].State, "a listed instance stays recorded")
	_, ok, err = store.Get(flatCRScionPr, "1a2b3c4d-5e6f-4071-8293-a4b5c6d7e8f9")
	require.NoError(t, err)
	assert.False(t, ok, "another instance's object is not this instance's")

	legacy := flatCRInstance("us-central1", "legacy-0123456789", "", "aa2b3c4d-5e6f-4071-8293-a4b5c6d7e8f9", "legacy", "31436587-a9cb-4fed-8102-132435465768")
	err = flatOwnershipPreflight(ctx, candidate(&flatCRInstances{instances: []*runpb.Instance{owned, legacy}}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), flatCRScionPr+"/legacy", "the unlabeled object is named by its project and slug")
}

// TestFlatOwnershipPreflight_LegacyCloudRunAgentRefusesScope: an agent
// instance without an owner label (for example one a legacy Runtime Broker
// created) on a project and region refuses every configured instance on
// that scope, and an instance in another region still activates.
func TestFlatOwnershipPreflight_LegacyCloudRunAgentRefusesScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	legacy := flatCRInstance("us-central1", "legacy-0123456789", "", "aa2b3c4d-5e6f-4071-8293-a4b5c6d7e8f9", "legacy", "31436587-a9cb-4fed-8102-132435465768")
	fakes := map[string]*flatCRInstances{
		"us-central1":  {instances: []*runpb.Instance{legacy}},
		"europe-west4": {},
	}
	insts := []config.V1RuntimeBrokerInstanceConfig{
		{Key: "cr-a", Name: "a", RuntimeTarget: flatCRTarget("us-central1")},
		{Key: "cr-b", Name: "b", RuntimeTarget: flatCRTarget("us-central1")},
		{Key: "cr-c", Name: "c", RuntimeTarget: flatCRTarget("europe-west4")},
	}
	// Distinct credential files per instance, as each needs its own.
	insts[1].RuntimeTarget.CredentialsFile = "/etc/scion/cr-b.json"
	act := &recordingFlatActivator{}
	h, err := brokerhost.New(brokerhost.Config{
		GlobalDir: t.TempDir(),
		Instances: insts,
		Mode:      brokerhost.ModeColocated,
		NewRuntime: func(_ context.Context, in config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) {
			return flatCRRuntime(t, in.RuntimeTarget.Region, fakes[in.RuntimeTarget.Region]), nil
		},
		ProbeScope: func(_ context.Context, in config.V1RuntimeBrokerInstanceConfig, _ runtime.Runtime) (brokeridentity.ExecutionScope, error) {
			return brokeridentity.ExecutionScope{Type: brokeridentity.TargetTypeCloudRun, CloudRun: &brokeridentity.CloudRunScope{
				ProjectNumber: "123456789012", Region: in.RuntimeTarget.Region, ProjectID: in.RuntimeTarget.ProjectID}}, nil
		},
		Activator: act,
		BuildServer: func(brokerhost.InstanceContext) (*runtimebroker.Server, error) {
			return nil, errors.New("not serving in this test")
		},
		OwnershipPreflight: flatOwnershipPreflight,
	})
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))

	st := map[string]brokerhost.InstanceStatus{}
	for _, s := range h.Status() {
		st[s.Key] = s
	}
	for _, k := range []string{"cr-a", "cr-b"} {
		assert.Equal(t, brokerhost.StateRefused, st[k].State, k)
		assert.Equal(t, "ownership_unresolved", st[k].Reason, k)
	}
	for _, k := range []string{"cr-a", "cr-b"} {
		assert.Contains(t, st[k].Error, flatCRScionPr+"/legacy", k)
	}
	assert.NotEqual(t, "ownership_unresolved", st["cr-c"].Reason, "another region is a different scope")
	assert.Contains(t, act.activated, "cr-c")
	assert.NotContains(t, act.activated, "cr-a")
	assert.NotContains(t, act.activated, "cr-b")
}

// TestNewFlatInstanceRuntime_CloudRunNotBuilt: the per-instance runtime
// factory does not build a cloudrun runtime in this release, so a valid
// cloudrun instance is refused before any credential is read or any scope
// is probed.
func TestNewFlatInstanceRuntime_CloudRunNotBuilt(t *testing.T) {
	rt, err := newFlatInstanceRuntime(context.Background(), config.V1RuntimeBrokerInstanceConfig{Key: "cr-a", Name: "a", RuntimeTarget: flatCRTarget("us-central1")})
	require.Error(t, err)
	assert.Nil(t, rt)
	assert.Contains(t, err.Error(), `runtime target type "cloudrun" is not supported in this release`)
	assert.NotContains(t, err.Error(), "/etc/scion", "the refusal names no credentials path")
}

// TestDoctor_FlatCloudRunNoHomesWarning: with a flat cloudrun instance
// configured, project_storage.homes is required, so neither the broker
// start-up warning nor scion doctor's Cloud Run check reports it as not
// applying (and doctor never advises removing it). Without one, the
// warning stays as it is for the legacy cloudrun runtime.
func TestDoctor_FlatCloudRunNoHomesWarning(t *testing.T) {
	// Both settings carry the same valid project_storage with homes set; they
	// differ only by the flat cloudrun instance. (nfs homes need an nfs block
	// with a share; without one validation fails and no warning is given at
	// all.)
	storage := func() *config.V1ProjectStorageConfig {
		return &config.V1ProjectStorageConfig{Backend: "local", Homes: "local"}
	}
	flat := &config.V1ServerConfig{
		ProjectStorage: storage(),
		Broker: &config.V1BrokerConfig{Instances: []config.V1RuntimeBrokerInstanceConfig{
			{Key: "docker-a", Name: "d", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeDocker}},
			{Key: "cr-a", Name: "a", RuntimeTarget: flatCRTarget("us-central1")},
		}},
	}
	legacy := &config.V1ServerConfig{
		ProjectStorage: storage(),
		Broker: &config.V1BrokerConfig{Instances: []config.V1RuntimeBrokerInstanceConfig{
			{Key: "docker-a", Name: "d", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeDocker}},
		}},
	}

	// The broker start-up path (server_foreground) logs this value.
	assert.Empty(t, runtime.CloudRunHomesWarningFor("cloudrun", flat))
	assert.Equal(t, runtime.CloudRunHomesWarning, runtime.CloudRunHomesWarningFor("cloudrun", legacy))

	// The doctor path runs the Cloud Run diagnostics with the global server
	// settings.
	rt, err := runtime.NewCloudRunRuntimeForTest(&config.CloudRunConfig{ProjectID: flatCRProject, Location: "us-central1"},
		func(context.Context) (cloudrun.InstancesAPI, error) { return &flatCRInstances{}, nil })
	require.NoError(t, err)
	homesChecks := func(srv *config.V1ServerConfig) []runtime.CheckResult {
		var out []runtime.CheckResult
		for _, c := range rt.RunDiagnostics(runtime.DiagnosticOpts{Server: srv}).Checks {
			if c.Message == runtime.CloudRunHomesWarning || strings.Contains(c.Remediation, "remove the setting") {
				out = append(out, c)
			}
		}
		return out
	}
	assert.Empty(t, homesChecks(flat), "no homes warning or removal advice with a flat cloudrun instance")
	assert.Len(t, homesChecks(legacy), 1, "the legacy warning is kept")
}
