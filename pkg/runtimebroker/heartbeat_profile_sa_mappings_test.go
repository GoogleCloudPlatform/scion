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
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The heartbeat sends per-profile GSA mappings on the first successful
// heartbeat and again only when they change; a failed send is retried.
func TestHeartbeat_ProfileSAMappingsSentOnFirstAndOnChange(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	hb := NewHeartbeatService(client, "test-host", time.Hour, &mockManager{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	current := []hubclient.ProfileSAMappingsState{{Name: "k8s", ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{{GSA: "a@p.iam.gserviceaccount.com"}}}}
	hb.profileSAMappings = func() []hubclient.ProfileSAMappingsState { return current }

	send := func() *hubclient.BrokerHeartbeat {
		t.Helper()
		_ = hb.sendHeartbeat(context.Background())
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.heartbeatCalls[len(client.heartbeatCalls)-1].Heartbeat
	}

	assert.Equal(t, current, send().ProfileSAMappings, "first heartbeat carries the mappings")
	assert.Nil(t, send().ProfileSAMappings, "unchanged mappings are not resent")

	current = []hubclient.ProfileSAMappingsState{{Name: "k8s", ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{}}}
	client.heartbeatErr = errors.New("hub down")
	assert.Equal(t, current, send().ProfileSAMappings, "a change is sent")
	client.heartbeatErr = nil
	assert.Equal(t, current, send().ProfileSAMappings, "a failed send is retried on the next heartbeat")
	assert.Nil(t, send().ProfileSAMappings)

	// Unreadable settings (nil) send nothing and do not reset the state.
	hb.profileSAMappings = func() []hubclient.ProfileSAMappingsState { return nil }
	assert.Nil(t, send().ProfileSAMappings)
}

func TestServer_HeartbeatProfileSAMappings(t *testing.T) {
	prev := loadHeartbeatMappingSettings
	t.Cleanup(func() { loadHeartbeatMappingSettings = prev })
	loadHeartbeatMappingSettings = func() (*config.VersionedSettings, error) {
		return &config.VersionedSettings{
			Profiles: map[string]config.V1ProfileConfig{
				"local": {Runtime: "docker"},
				"gke":   {Runtime: "gke-entry", KubernetesServiceAccountMappings: map[string]string{"p@x.iam.gserviceaccount.com": "ksa"}},
				"k8s":   {Runtime: "kubernetes"},
			},
			Runtimes: map[string]config.V1RuntimeConfig{
				"docker":     {Type: "docker"},
				"gke-entry":  {Type: "kubernetes", KubernetesServiceAccountMappings: map[string]string{"r@x.iam.gserviceaccount.com": "ksa"}},
				"kubernetes": {Type: "kubernetes"},
			},
		}, nil
	}
	srv := &Server{}
	got := srv.heartbeatProfileSAMappings()
	require.Equal(t, []hubclient.ProfileSAMappingsState{
		{Name: "gke", ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{{GSA: "p@x.iam.gserviceaccount.com"}, {GSA: "r@x.iam.gserviceaccount.com"}}},
		{Name: "k8s", ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{}},
	}, got, "Kubernetes profiles only (custom key by type), sorted, empty list kept")

	loadHeartbeatMappingSettings = func() (*config.VersionedSettings, error) { return nil, errors.New("bad") }
	assert.Nil(t, srv.heartbeatProfileSAMappings(), "unreadable settings report nothing")
}
