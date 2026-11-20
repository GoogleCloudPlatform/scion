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
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func wiTestSettings() *config.VersionedSettings {
	return &config.VersionedSettings{
		Profiles: map[string]config.V1ProfileConfig{
			"local":  {Runtime: "docker"},
			"gke":    {Runtime: "gke-entry"},
			"gke-2":  {Runtime: "gke-entry"},
			"nopool": {Runtime: "kubernetes"},
			"bad":    {Runtime: "bad-entry"},
			"dock":   {Runtime: "docker-pool"},
		},
		Runtimes: map[string]config.V1RuntimeConfig{
			"docker":      {Type: "docker"},
			"docker-pool": {Type: "docker", WorkloadIdentityPool: "cluster-project.svc.id.goog"},
			"gke-entry":   {Type: "kubernetes", Namespace: "team-a", WorkloadIdentityPool: " cluster-project.svc.id.goog "},
			"kubernetes":  {Type: "kubernetes"},
			"bad-entry":   {Type: "kubernetes", WorkloadIdentityPool: "cluster-project"},
		},
	}
}

// One target per Kubernetes profile whose runtime entry sets a valid pool,
// sorted by profile, with the namespace the dispatch resolves. Non-Kubernetes
// profiles, profiles without a pool and malformed pools are left out.
func TestWorkloadIdentityTargetsFrom(t *testing.T) {
	var invalid []string
	got := workloadIdentityTargetsFrom(wiTestSettings(), func(profile, pool string) {
		invalid = append(invalid, profile+"="+pool)
	})
	require.Equal(t, []hubclient.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "cluster-project.svc.id.goog", Namespace: "team-a"},
		{Profile: "gke-2", Pool: "cluster-project.svc.id.goog", Namespace: "team-a"},
	}, got)
	assert.Equal(t, []string{"bad=cluster-project"}, invalid)

	empty := workloadIdentityTargetsFrom(&config.VersionedSettings{}, nil)
	require.NotNil(t, empty, "no targets is an empty list, not nil")
	assert.Empty(t, empty)
}

// The namespace falls back to the runtime's default namespace, as at
// dispatch.
func TestWorkloadIdentityTargetsFrom_DefaultNamespace(t *testing.T) {
	t.Setenv("SCION_K8S_NAMESPACE", "default-ns")
	got := workloadIdentityTargetsFrom(&config.VersionedSettings{
		Profiles: map[string]config.V1ProfileConfig{"k8s": {Runtime: "kubernetes"}},
		Runtimes: map[string]config.V1RuntimeConfig{"kubernetes": {Type: "kubernetes", WorkloadIdentityPool: "cluster-project.svc.id.goog"}},
	}, nil)
	require.Equal(t, []hubclient.WorkloadIdentityTarget{{Profile: "k8s", Pool: "cluster-project.svc.id.goog", Namespace: "default-ns"}}, got)
}

func TestServer_HeartbeatWorkloadIdentityTargets(t *testing.T) {
	srv := &Server{config: ServerConfig{}}
	srv.saDiscoveryCache = newSADiscoveryCache(nil, discardLogger())
	srv.loadMappingSettings = func() (*config.VersionedSettings, error) { return wiTestSettings(), nil }
	got := srv.heartbeatWorkloadIdentityTargets()
	require.Len(t, got, 2)

	srv.config.ForceRuntime = "kubernetes"
	forced := srv.heartbeatWorkloadIdentityTargets()
	require.NotNil(t, forced)
	assert.Empty(t, forced, "ForceRuntime: profiles do not apply, so no target")

	srv.loadMappingSettings = func() (*config.VersionedSettings, error) { return nil, errors.New("bad") }
	assert.Nil(t, srv.heartbeatWorkloadIdentityTargets(), "unreadable settings report nil, so the hub keeps its stored list")
}

// A legacy heartbeat carries the targets and the wi_targets_v1 capability;
// an empty list is sent as [] and an unreadable one as null. Without a
// producer, or on a flat instance, neither is sent.
func TestHeartbeat_ReportsWorkloadIdentityTargets(t *testing.T) {
	hb := NewHeartbeatService(&mockRuntimeBrokerService{}, "test-host", time.Hour, &mockManager{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	got := hb.buildHeartbeat(context.Background())
	assert.Nil(t, got.WorkloadIdentityTargets)
	assert.False(t, got.Capabilities.WITargetsV1, "no producer, no capability")

	want := []hubclient.WorkloadIdentityTarget{{Profile: "gke", Pool: "p.svc.id.goog", Namespace: "agents"}}
	hb.workloadIdentityTargets = func() []hubclient.WorkloadIdentityTarget { return want }
	got = hb.buildHeartbeat(context.Background())
	assert.Equal(t, want, got.WorkloadIdentityTargets)
	assert.True(t, got.Capabilities.WITargetsV1)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"workloadIdentityTargets":[{"profile":"gke","pool":"p.svc.id.goog","namespace":"agents"}]`)
	assert.Contains(t, string(raw), `"wiTargetsV1":true`)

	hb.workloadIdentityTargets = func() []hubclient.WorkloadIdentityTarget { return []hubclient.WorkloadIdentityTarget{} }
	raw, err = json.Marshal(hb.buildHeartbeat(context.Background()))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"workloadIdentityTargets":[]`, "none configured is sent as an empty list")

	hb.workloadIdentityTargets = func() []hubclient.WorkloadIdentityTarget { return nil }
	raw, err = json.Marshal(hb.buildHeartbeat(context.Background()))
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"workloadIdentityTargets":null`, "unreadable is sent as null")

	hb.workloadIdentityTargets = func() []hubclient.WorkloadIdentityTarget { return want }
	hb.flat = true
	got = hb.buildHeartbeat(context.Background())
	assert.Nil(t, got.WorkloadIdentityTargets, "a flat instance has no producer yet (phase 5 P3b)")
	assert.False(t, got.Capabilities.WITargetsV1)
}

func TestWorkloadIdentityTargetsFrom_KSACreation(t *testing.T) {
	vs := &config.VersionedSettings{
		Profiles: map[string]config.V1ProfileConfig{
			"both":        {Runtime: "rt-both"},
			"create-only": {Runtime: "rt-create"},
			"bad-pool":    {Runtime: "rt-bad-pool"},
			"docker":      {Runtime: "rt-docker"},
			"neither":     {Runtime: "rt-neither"},
		},
		Runtimes: map[string]config.V1RuntimeConfig{
			"rt-both":     {Type: "kubernetes", Namespace: "agents", WorkloadIdentityPool: "cluster-project.svc.id.goog", CreateServiceAccounts: true},
			"rt-create":   {Type: "kubernetes", Namespace: "agents", CreateServiceAccounts: true},
			"rt-bad-pool": {Type: "kubernetes", Namespace: "agents", WorkloadIdentityPool: "not-a-pool", CreateServiceAccounts: true},
			"rt-docker":   {Type: "docker", CreateServiceAccounts: true},
			"rt-neither":  {Type: "kubernetes", Namespace: "agents"},
		},
	}
	var invalid []string
	got := workloadIdentityTargetsFrom(vs, func(profile, pool string) {
		invalid = append(invalid, profile+"="+pool)
	})
	require.Equal(t, []hubclient.WorkloadIdentityTarget{
		{Profile: "bad-pool", Pool: "", Namespace: "agents", KSACreation: true},
		{Profile: "both", Pool: "cluster-project.svc.id.goog", Namespace: "agents", KSACreation: true},
		{Profile: "create-only", Pool: "", Namespace: "agents", KSACreation: true},
	}, got)
	assert.Equal(t, []string{"bad-pool=not-a-pool"}, invalid)

	raw, err := json.Marshal(got[1])
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"ksaCreation":true`)

	rawFalse, err := json.Marshal(hubclient.WorkloadIdentityTarget{Profile: "p", Pool: "cluster-project.svc.id.goog", Namespace: "agents"})
	require.NoError(t, err)
	assert.NotContains(t, string(rawFalse), "ksaCreation")
}
