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

package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Cloud Run Instances stores label keys GCP-sanitized (dots become
// underscores), so its owner label is read and written under the
// sanitized key; other runtimes keep api.LabelRuntimeBrokerID
// (ptone/scion#5271).

const cloudRunOwnerKey = "scion_runtime_broker_id"

func ownedCloudRunManager(brokerID string) *AgentManager {
	m := NewManager(&runtime.CloudRunRuntime{}).(*AgentManager)
	m.SetOwner(OwnerScope{RuntimeBrokerID: brokerID})
	return m
}

func cloudRunEntry(labels map[string]string) api.AgentInfo {
	return api.AgentInfo{ID: "agent-1", Name: "projects/p/locations/l/instances/agent-1", Labels: labels}
}

func TestOwner_CloudRun_OwnInstanceIsOwned(t *testing.T) {
	m := ownedCloudRunManager("broker-a")
	assert.True(t, m.ownsEntry(cloudRunEntry(map[string]string{"scion_name": "worker", cloudRunOwnerKey: "broker-a"})),
		"an instance carrying this instance's ID under the sanitized owner key is owned")
}

func TestOwner_CloudRun_ForeignInstanceIsNotOwned(t *testing.T) {
	m := ownedCloudRunManager("broker-a")
	assert.False(t, m.ownsEntry(cloudRunEntry(map[string]string{"scion_name": "worker", cloudRunOwnerKey: "broker-b"})),
		"an instance labelled for another Runtime Broker instance is not owned")
}

func TestOwner_CloudRun_UnlabelledInstanceIsNotOwned(t *testing.T) {
	m := ownedCloudRunManager("broker-a")
	assert.False(t, m.ownsEntry(cloudRunEntry(map[string]string{"scion_name": "worker"})), "no owner label")
	assert.False(t, m.ownsEntry(cloudRunEntry(map[string]string{"scion_name": "worker", cloudRunOwnerKey: ""})), "empty owner label")
	assert.False(t, m.ownsEntry(cloudRunEntry(map[string]string{"scion_name": "worker", api.LabelRuntimeBrokerID: "broker-a"})),
		"Cloud Run reads only its sanitized owner key, never the unsanitized one")

	// A manager whose instance ID is empty owns no unlabelled or
	// empty-labelled instance either.
	empty := ownedCloudRunManager("")
	assert.False(t, empty.ownsEntry(cloudRunEntry(map[string]string{"scion_name": "worker"})), "no owner label, empty instance ID")
	assert.False(t, empty.ownsEntry(cloudRunEntry(map[string]string{cloudRunOwnerKey: ""})), "empty owner label, empty instance ID")
}

func TestOwner_NonSanitizingRuntimeUnchanged(t *testing.T) {
	rt := &runtime.MockRuntime{ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return []api.AgentInfo{
			{ContainerID: "c-dotted", Labels: map[string]string{api.LabelRuntimeBrokerID: "broker-a"}},
			{ContainerID: "c-sanitized", Labels: map[string]string{cloudRunOwnerKey: "broker-a"}},
			{ContainerID: "c-none", Labels: map[string]string{}},
		}, nil
	}}
	m := NewManager(rt).(*AgentManager)
	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a"})

	owned, err := m.listRuntime(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, owned, 1)
	assert.Equal(t, "c-dotted", owned[0].ContainerID, "other runtimes read only api.LabelRuntimeBrokerID")
}

func TestOwner_OwnerLabelsUseTheRuntimesKey(t *testing.T) {
	cr := ownedCloudRunManager("broker-a")
	assert.Equal(t, map[string]string{cloudRunOwnerKey: "broker-a"}, cr.ownerLabels(),
		"Cloud Run stamps the owner label under the key its filter reads")
	assert.Equal(t, cloudRunOwnerKey, runtime.LabelKey(cr.Runtime, api.LabelRuntimeBrokerID))

	m := NewManager(&runtime.MockRuntime{}).(*AgentManager)
	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a"})
	assert.Equal(t, map[string]string{api.LabelRuntimeBrokerID: "broker-a"}, m.ownerLabels(),
		"other runtimes stamp api.LabelRuntimeBrokerID")
}

// TestOwner_CloudRun_UnresolvedReadsThroughRuntimeKeys: the unresolved
// check gets the runtime's label key mapping, so a contested Cloud Run
// instance (GCP-sanitized keys) carrying this instance's owner label is
// not owned, and an uncontested one is.
func TestOwner_CloudRun_UnresolvedReadsThroughRuntimeKeys(t *testing.T) {
	m := NewManager(&runtime.CloudRunRuntime{}).(*AgentManager)
	m.SetOwner(OwnerScope{RuntimeBrokerID: "broker-a", EntryUnresolved: func(l map[string]string, key func(string) string) bool {
		return l[key("scion.project_id")] == "proj" && l[key("scion.name")] == "contested"
	}})
	contested := cloudRunEntry(map[string]string{cloudRunOwnerKey: "broker-a", "scion_project_id": "proj", "scion_name": "contested"})
	uncontested := cloudRunEntry(map[string]string{cloudRunOwnerKey: "broker-a", "scion_project_id": "proj", "scion_name": "other"})
	assert.False(t, m.ownsEntry(contested), "a contested Cloud Run instance is owned")
	assert.True(t, m.ownsEntry(uncontested), "an uncontested Cloud Run instance is not owned")
}

// TestOwner_EntryPathNeedsTheProjectID: List's write gate reads the
// project ID label under the runtime's key for it, and refuses an entry
// without one before consulting EntryPathTrusted.
func TestOwner_EntryPathNeedsTheProjectID(t *testing.T) {
	var asked []string
	scope := OwnerScope{RuntimeBrokerID: "broker-a", EntryPathTrusted: func(path, projectID string) bool {
		asked = append(asked, projectID)
		return true
	}}
	cr := NewManager(&runtime.CloudRunRuntime{}).(*AgentManager)
	cr.SetOwner(scope)
	entry := api.AgentInfo{ProjectPath: "/p", Labels: map[string]string{cloudRunOwnerKey: "broker-a", "scion_project_id": "proj-1"}}
	assert.True(t, cr.mayWriteEntryPath(entry))
	assert.Equal(t, []string{"proj-1"}, asked, "Cloud Run reads the project ID under its sanitized key")

	asked = nil
	for name, labels := range map[string]map[string]string{
		"Cloud Run, no project ID":       {cloudRunOwnerKey: "broker-a"},
		"Cloud Run, empty project ID":    {cloudRunOwnerKey: "broker-a", "scion_project_id": ""},
		"Cloud Run, only the dotted key": {cloudRunOwnerKey: "broker-a", "scion.project_id": "proj-1"},
	} {
		assert.False(t, cr.mayWriteEntryPath(api.AgentInfo{ProjectPath: "/p", Labels: labels}), name)
	}
	m := NewManager(&runtime.MockRuntime{}).(*AgentManager)
	m.SetOwner(scope)
	assert.False(t, m.mayWriteEntryPath(api.AgentInfo{ProjectPath: "/p", Labels: map[string]string{api.LabelRuntimeBrokerID: "broker-a"}}),
		"other runtimes: no project ID")
	assert.Empty(t, asked, "EntryPathTrusted is consulted without a project ID")
}
