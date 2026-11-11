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
	"strings"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
)

// Ownership of Cloud Run objects: labels read under the keys Cloud Run
// stores them under, handles equal to the launch's, exact values.

const (
	crOwnBroker  = "6c0f9a3e-8d1b-4e2f-a5c7-3b9d0e1f2a4c"
	crOwnProject = "5a4b3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d"
	crOwnAgent   = "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"
	crOwnRun     = "0f1e2d3c-4b5a-4968-8776-655443322110"
	crOwnParent  = "projects/test-project/locations/us-central1"
	crOwnID      = "worker-0123456789"
)

// crOwnedInstance is an owned Cloud Run instance as CloudRunRuntime.Run
// stores it: every label key GCP-sanitized, values as given.
func crOwnedInstance(instanceID, slug, runID string) *runpb.Instance {
	return &runpb.Instance{
		Name: crInstanceName(instanceID),
		Labels: map[string]string{
			"scion_runtime_broker_id": crOwnBroker,
			"scion_agent":             "true",
			"scion_project_id":        crOwnProject,
			"agent_id":                crOwnAgent,
			"scion_name":              slug,
			"scion_run_id":            runID,
		},
	}
}

// crOwnRuntime is a real CloudRunRuntime over the fake instances.
func crOwnRuntime(t *testing.T, fake *crInstances) *runtime.CloudRunRuntime {
	t.Helper()
	rt, err := runtime.NewCloudRunRuntimeForTest(&config.CloudRunConfig{ProjectID: "test-project", Location: "us-central1"},
		func(context.Context) (cloudrun.InstancesAPI, error) { return fake, nil })
	require.NoError(t, err)
	return rt
}

// crOwnListed lists the fake's instances through rt, as the preflight does.
func crOwnListed(t *testing.T, rt runtime.Runtime) []api.AgentInfo {
	t.Helper()
	listed, err := rt.List(context.Background(), map[string]string{"scion.agent": "true"})
	require.NoError(t, err)
	return listed
}

// crLaunchHandle is the handle CloudRunRuntime.Run records for crOwnID.
func crLaunchHandle(runID string) api.ResourceHandle {
	return api.ResourceHandle{Kind: api.ResourceKindCloudRunInstance, Namespace: crOwnParent, Name: crOwnID, UID: runID}
}

// TestOwnershipReconstruct_UsesRuntimeLabelKey: an owned Cloud Run
// instance, whose labels are stored under sanitized keys, reconstructs its
// record when read through the runtime's label keys, as the launch
// recorded it; read under the raw keys it would not be this instance's.
func TestOwnershipReconstruct_UsesRuntimeLabelKey(t *testing.T) {
	rt := crOwnRuntime(t, &crInstances{instances: []*runpb.Instance{crOwnedInstance(crOwnID, "worker", crOwnRun)}})
	listed := crOwnListed(t, rt)
	require.Len(t, listed, 1)

	s := NewOwnershipStore(t.TempDir(), crOwnBroker)
	require.Error(t, s.Reconstruct(listed[0], nil), "raw keys do not find the stored owner label")
	require.NoError(t, s.Reconstruct(listed[0], rt))
	rec, ok, err := s.Get(crOwnProject, crOwnAgent)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "worker", rec.AgentSlug)
	run := rec.Run(crOwnRun)
	require.NotNil(t, run)
	assert.Equal(t, OwnershipStateCreated, run.State)
	assert.Equal(t, []OwnedResource{{Kind: api.ResourceKindCloudRunInstance, Namespace: crOwnParent, Name: crOwnID,
		UID: crOwnRun, State: OwnedResourceRecorded}}, run.Resources)

	// Docker and Kubernetes keep the raw keys: the same function reads them.
	docker := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	assert.Equal(t, api.LabelRuntimeBrokerID, runtime.LabelKey(docker, api.LabelRuntimeBrokerID))
	assert.Equal(t, "scion_runtime_broker_id", runtime.LabelKey(rt, api.LabelRuntimeBrokerID))
}

// TestOwnershipReconstruct_CloudRunHandleMatchesLaunch: reconstructing a
// launched instance adds no second resource, and repeating it is a no-op.
func TestOwnershipReconstruct_CloudRunHandleMatchesLaunch(t *testing.T) {
	rt := crOwnRuntime(t, &crInstances{instances: []*runpb.Instance{crOwnedInstance(crOwnID, "worker", crOwnRun)}})
	s := NewOwnershipStore(t.TempDir(), crOwnBroker)
	require.NoError(t, s.BeginRun(crOwnProject, crOwnAgent, "worker", crOwnRun))
	require.NoError(t, s.AddResource(crOwnProject, crOwnAgent, crOwnRun, crLaunchHandle(crOwnRun)))
	require.NoError(t, s.SetRunState(crOwnProject, crOwnAgent, crOwnRun, OwnershipStateCreated))

	listed := crOwnListed(t, rt)
	require.Len(t, listed, 1)
	assert.Equal(t, crLaunchHandle(crOwnRun), rt.ListedObjectHandle(listed[0]))
	for i := 0; i < 2; i++ {
		require.NoError(t, s.Reconstruct(listed[0], rt))
	}
	rec, _, err := s.Get(crOwnProject, crOwnAgent)
	require.NoError(t, err)
	assert.Len(t, rec.Run(crOwnRun).Resources, 1, "the launched and the reconstructed handle are one resource")
}

// TestOwnershipReconstruct_CloudRunSlugAtBound: a canonical slug of
// api.MaxSlugLength characters goes through the label rewrite Run applies
// (runtime.CloudRunStoredLabels) exactly as given, and the stored instance
// reconstructs.
func TestOwnershipReconstruct_CloudRunSlugAtBound(t *testing.T) {
	slug := strings.Repeat("w", api.MaxSlugLength-2) + "-1"
	require.Len(t, slug, 63)
	stored := runtime.CloudRunStoredLabels(map[string]string{
		api.LabelRuntimeBrokerID: crOwnBroker, "scion.agent": "true", "scion.project_id": crOwnProject,
		"agent_id": crOwnAgent, "scion.name": slug, api.LabelRunID: crOwnRun,
	})
	require.Equal(t, slug, stored["scion_name"], "the rewrite leaves a canonical 63-character slug as given")
	rt := crOwnRuntime(t, &crInstances{instances: []*runpb.Instance{{Name: crInstanceName(crOwnID), Labels: stored}}})
	s := NewOwnershipStore(t.TempDir(), crOwnBroker)
	require.NoError(t, s.BeginRun(crOwnProject, crOwnAgent, slug, crOwnRun))
	listed := crOwnListed(t, rt)
	require.Len(t, listed, 1)
	require.NoError(t, s.Reconstruct(listed[0], rt))
	rec, _, err := s.Get(crOwnProject, crOwnAgent)
	require.NoError(t, err)
	assert.Equal(t, slug, rec.AgentSlug)
	holder, err := s.SlugHolder(crOwnProject, slug)
	require.NoError(t, err)
	assert.Equal(t, crOwnAgent, holder)
}

// TestOwnershipReconstruct_SanitizedValueUnresolved: a stored label value
// that differs from the record only by the GCP rewrite (lower-casing or
// truncation) is unresolved: it is never matched to the record and never
// starts a second record for the same run.
func TestOwnershipReconstruct_SanitizedValueUnresolved(t *testing.T) {
	longProject := crOwnProject + "__" + strings.Repeat("p", 40)
	for name, tc := range map[string]struct {
		recProject, recAgent, recSlug string
		instance                      *runpb.Instance
	}{
		"truncated project ID": {longProject, crOwnAgent, "worker", func() *runpb.Instance {
			inst := crOwnedInstance(crOwnID, "worker", crOwnRun)
			inst.Labels["scion_project_id"] = longProject[:63]
			return inst
		}()},
		"lower-cased agent ID": {crOwnProject, strings.ToUpper(crOwnAgent), "worker", crOwnedInstance(crOwnID, "worker", crOwnRun)},
		"lower-cased slug":     {crOwnProject, crOwnAgent, "Worker", crOwnedInstance(crOwnID, "worker", crOwnRun)},
	} {
		t.Run(name, func(t *testing.T) {
			s := NewOwnershipStore(t.TempDir(), crOwnBroker)
			require.NoError(t, s.BeginRun(tc.recProject, tc.recAgent, tc.recSlug, crOwnRun))
			rt := crOwnRuntime(t, &crInstances{instances: []*runpb.Instance{tc.instance}})
			listed := crOwnListed(t, rt)
			require.Len(t, listed, 1)
			require.Error(t, s.Reconstruct(listed[0], rt))
			recs, err := s.List()
			require.NoError(t, err)
			require.Len(t, recs, 1, "no second record is started for the run")
			assert.Equal(t, tc.recProject, recs[0].ProjectID)
			assert.Equal(t, tc.recAgent, recs[0].AgentID)
			assert.Empty(t, recs[0].Run(crOwnRun).Resources, "the object is not attributed to the record")
		})
	}
}

// TestReconcileAbsent_CloudRunListed: a recorded Cloud Run instance that
// the complete listing shows with its recorded run ID is present: it stays
// recorded and needs no exact check.
func TestReconcileAbsent_CloudRunListed(t *testing.T) {
	rt := crOwnRuntime(t, &crInstances{instances: []*runpb.Instance{crOwnedInstance(crOwnID, "worker", crOwnRun)}})
	s := NewOwnershipStore(t.TempDir(), crOwnBroker)
	require.NoError(t, s.BeginRun(crOwnProject, crOwnAgent, "worker", crOwnRun))
	require.NoError(t, s.AddResource(crOwnProject, crOwnAgent, crOwnRun, crLaunchHandle(crOwnRun)))
	require.NoError(t, s.SetRunState(crOwnProject, crOwnAgent, crOwnRun, OwnershipStateCreated))

	changed, err := s.ReconcileAbsent(crOwnListed(t, rt), func(h api.ResourceHandle) (bool, error) {
		t.Fatalf("exact check called for a listed main object: %+v", h)
		return false, nil
	}, nil)
	require.NoError(t, err)
	assert.Zero(t, changed)
	rec, _, err := s.Get(crOwnProject, crOwnAgent)
	require.NoError(t, err)
	assert.Equal(t, OwnedResourceRecorded, rec.Run(crOwnRun).Resources[0].State)
}

// TestReconcileAbsent_CloudRunUnlisted: a recorded Cloud Run instance the
// listing does not show, or shows only under another run's ID, is a main
// object decided by the runtime's exact check, which compares the run ID
// label; a confirmed absence finishes a deleting run.
func TestReconcileAbsent_CloudRunUnlisted(t *testing.T) {
	for name, instances := range map[string][]*runpb.Instance{
		"not listed":                 nil,
		"another run holds the name": {crOwnedInstance(crOwnID, "worker", "11111111-2222-4333-8444-555555555555")},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &crInstances{instances: instances}
			rt := crOwnRuntime(t, fake)
			s := NewOwnershipStore(t.TempDir(), crOwnBroker)
			require.NoError(t, s.BeginRun(crOwnProject, crOwnAgent, "worker", crOwnRun))
			require.NoError(t, s.AddResource(crOwnProject, crOwnAgent, crOwnRun, crLaunchHandle(crOwnRun)))
			require.NoError(t, s.SetRunState(crOwnProject, crOwnAgent, crOwnRun, OwnershipStateDeleting))

			checked := 0
			changed, err := s.ReconcileAbsent(crOwnListed(t, rt), func(h api.ResourceHandle) (bool, error) {
				checked++
				return rt.ResourceAbsent(context.Background(), h)
			}, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, checked, "the exact check decides")
			assert.Equal(t, 2, changed, "marked absent, then the deleting run finished")
			rec, _, err := s.Get(crOwnProject, crOwnAgent)
			require.NoError(t, err)
			assert.Equal(t, OwnedResourceAbsent, rec.Run(crOwnRun).Resources[0].State)
			assert.Equal(t, OwnershipStateDeleted, rec.Run(crOwnRun).State)
		})
	}

	// A listed instance with the recorded run ID keeps a deleting run's
	// object recorded: the exact check finds it present.
	rt := crOwnRuntime(t, &crInstances{instances: []*runpb.Instance{crOwnedInstance(crOwnID, "worker", crOwnRun)}})
	s := NewOwnershipStore(t.TempDir(), crOwnBroker)
	require.NoError(t, s.BeginRun(crOwnProject, crOwnAgent, "worker", crOwnRun))
	require.NoError(t, s.AddResource(crOwnProject, crOwnAgent, crOwnRun, crLaunchHandle(crOwnRun)))
	require.NoError(t, s.SetRunState(crOwnProject, crOwnAgent, crOwnRun, OwnershipStateDeleting))
	_, err := s.ReconcileAbsent(crOwnListed(t, rt), func(h api.ResourceHandle) (bool, error) {
		return rt.ResourceAbsent(context.Background(), h)
	}, nil)
	require.NoError(t, err)
	rec, _, err := s.Get(crOwnProject, crOwnAgent)
	require.NoError(t, err)
	assert.Equal(t, OwnedResourceRecorded, rec.Run(crOwnRun).Resources[0].State)
	assert.Equal(t, OwnershipStateDeleting, rec.Run(crOwnRun).State)
}

// TestFlatServer_CloudRunHasNoOwnershipSetupErr: a flat server over the
// Cloud Run runtime can confirm deleted objects are gone, so it has no
// ownership setup error; a runtime without the exact check still has one.
func TestFlatServer_CloudRunHasNoOwnershipSetupErr(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	flatCfg := func(key string) ServerConfig {
		cfg := testServerConfig()
		cfg.Host, cfg.Port = "127.0.0.1", 0
		cfg.BrokerID = crOwnBroker
		cfg.StateDir = t.TempDir()
		cfg.FlatInstance = &FlatInstanceConfig{
			Identity: &brokeridentity.Identity{InstanceKey: key, RuntimeBrokerID: crOwnBroker,
				RuntimeTarget: api.RuntimeTargetDescriptor{ID: "target-1", Type: brokeridentity.TargetTypeCloudRun}},
			Instance:     config.V1RuntimeBrokerInstanceConfig{Key: key, Name: key, RuntimeTarget: &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeCloudRun}},
			HubInProcess: true,
		}
		return cfg
	}
	rt := crOwnRuntime(t, &crInstances{})
	srv := New(flatCfg("cr-a"), agent.NewManager(rt), rt, config.RuntimeBuildRecord{})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	assert.NoError(t, srv.ownershipSetupErr)
	assert.NotNil(t, srv.ownership)

	mock := noAbsenceRuntime{&runtime.MockRuntime{NameFunc: func() string { return "cloudrun" }}}
	other := New(flatCfg("cr-b"), agent.NewManager(mock), mock, config.RuntimeBuildRecord{})
	t.Cleanup(func() { _ = other.Shutdown(context.Background()) })
	require.Error(t, other.ownershipSetupErr, "a runtime that cannot confirm absence still refuses to serve")
}
