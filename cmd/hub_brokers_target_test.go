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

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func TestFormatBrokerTargetCell(t *testing.T) {
	cases := []struct {
		name   string
		broker hubclient.RuntimeBroker
		want   string
	}{
		{
			name: "flat docker without display name",
			broker: hubclient.RuntimeBroker{
				RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "rt-1", Type: "docker"},
			},
			want: "docker",
		},
		{
			name: "flat docker with display name",
			broker: hubclient.RuntimeBroker{
				RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "rt-1", Type: "docker", DisplayName: "Workstation"},
			},
			want: "docker / Workstation",
		},
		{
			name: "flat kubernetes with display name and reported namespace",
			broker: hubclient.RuntimeBroker{
				RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "rt-2", Type: "kubernetes", DisplayName: "Cluster A"},
				RuntimeTargetPresentation: &api.RuntimeTargetPresentation{
					SchemaVersion: 1,
					Target:        &api.PresentedTarget{Type: "kubernetes", DisplayName: "Cluster A", Namespace: "agents"},
				},
			},
			want: "kubernetes / Cluster A / ns agents",
		},
		{
			name: "flat kubernetes without presentation",
			broker: hubclient.RuntimeBroker{
				RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "rt-2", Type: "kubernetes", DisplayName: "Cluster A"},
			},
			want: "kubernetes / Cluster A",
		},
		{
			name: "flat kubernetes with namespace and no display name",
			broker: hubclient.RuntimeBroker{
				RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "rt-2", Type: "kubernetes"},
				RuntimeTargetPresentation: &api.RuntimeTargetPresentation{
					SchemaVersion: 1,
					Target:        &api.PresentedTarget{Type: "kubernetes", Namespace: "agents"},
				},
			},
			want: "kubernetes / ns agents",
		},
		{
			name:   "legacy with no profiles",
			broker: hubclient.RuntimeBroker{},
			want:   "legacy",
		},
		{
			name: "legacy with 1 profile",
			broker: hubclient.RuntimeBroker{
				Profiles: []hubclient.BrokerProfile{{Name: "default", Type: "docker", Available: true}},
			},
			want: "legacy (1 profile)",
		},
		{
			name: "legacy with multiple profiles",
			broker: hubclient.RuntimeBroker{
				Profiles: []hubclient.BrokerProfile{
					{Name: "docker", Type: "docker", Available: true},
					{Name: "gke", Type: "kubernetes", Available: true},
				},
			},
			want: "legacy (2 profiles)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, formatBrokerTargetCell(tc.broker))
		})
	}
}

func TestRunHubBrokers_TableAndJSON(t *testing.T) {
	now := time.Now().UTC().Add(-2 * time.Minute)
	brokers := []hubclient.RuntimeBroker{
		{
			ID:            "b-flat-docker",
			Name:          "local-docker",
			Status:        "online",
			AutoProvide:   true,
			LastHeartbeat: now,
			Profiles:      []hubclient.BrokerProfile{},
			RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "rt-docker", Type: "docker", DisplayName: "Workstation"},
		},
		{
			ID:            "b-flat-k8s",
			Name:          "cluster-a-agents",
			Status:        "online",
			AutoProvide:   false,
			LastHeartbeat: now,
			Profiles:      []hubclient.BrokerProfile{},
			RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "rt-k8s", Type: "kubernetes", DisplayName: "Cluster A"},
			RuntimeTargetPresentation: &api.RuntimeTargetPresentation{
				SchemaVersion: 1,
				Key:           "cluster-a",
				Name:          "cluster-a-agents",
				Target:        &api.PresentedTarget{Type: "kubernetes", DisplayName: "Cluster A", Namespace: "agents"},
			},
		},
		{
			ID:          "b-legacy",
			Name:        "legacy-node",
			Status:      "online",
			AutoProvide: true,
			Profiles: []hubclient.BrokerProfile{
				{Name: "local", Type: "docker", Available: true},
				{Name: "gke", Type: "kubernetes", Available: true},
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/runtime-brokers" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"brokers": brokers})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	t.Chdir(home)

	origProjectPath, origFormat, origHubJSON, origGlobal := projectPath, outputFormat, hubOutputJSON, globalMode
	t.Cleanup(func() {
		projectPath = origProjectPath
		outputFormat = origFormat
		hubOutputJSON = origHubJSON
		globalMode = origGlobal
	})
	projectPath = ""
	globalMode = true

	t.Run("human table includes TARGET column", func(t *testing.T) {
		outputFormat = ""
		hubOutputJSON = false
		var err error
		stdout, _ := captureStdIO(t, func() {
			err = runHubBrokers(hubBrokersCmd, nil)
		})
		require.NoError(t, err)
		lines := strings.Split(strings.TrimSpace(stdout), "\n")
		require.GreaterOrEqual(t, len(lines), 5)
		assert.Contains(t, lines[0], "TARGET")
		assert.Contains(t, stdout, "docker / Workstation")
		assert.Contains(t, stdout, "kubernetes / Cluster A / ns agents")
		assert.Contains(t, stdout, "legacy (2 profiles)")
	})

	t.Run("json format preserves broker records unchanged", func(t *testing.T) {
		outputFormat = "json"
		hubOutputJSON = false
		var err error
		stdout, _ := captureStdIO(t, func() {
			err = runHubBrokers(hubBrokersCmd, nil)
		})
		require.NoError(t, err)
		var decoded []hubclient.RuntimeBroker
		require.NoError(t, json.Unmarshal([]byte(stdout), &decoded))
		require.Len(t, decoded, 3)
		require.NotNil(t, decoded[0].RuntimeTarget)
		assert.Equal(t, "docker", decoded[0].RuntimeTarget.Type)
		require.NotNil(t, decoded[1].RuntimeTargetPresentation)
		assert.Equal(t, "agents", decoded[1].RuntimeTargetPresentation.Target.Namespace)
		assert.Len(t, decoded[2].Profiles, 2)
	})
}

func TestWrapHubError_RuntimeProfileUnsupported(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "")
	base := &apiclient.APIError{
		StatusCode: http.StatusUnprocessableEntity,
		Code:       api.ErrCodeRuntimeProfileUnsupported,
		Message:    "Runtime Broker b-1 serves a single runtime target and does not accept Runtime Broker Profile gpu",
	}
	wrapped := wrapHubError(fmt.Errorf("failed to create agent via Hub: %w", base))
	require.Error(t, wrapped)
	msg := wrapped.Error()
	assert.Contains(t, msg, "does not accept Runtime Broker Profile gpu")
	assert.Contains(t, msg, "--broker")
	assert.Contains(t, msg, "--profile")
	assert.Contains(t, msg, "legacy")
	assert.NotContains(t, msg, "scion hub status")
	assert.True(t, errors.Is(wrapped, base))

	// Double-wrapping does not duplicate the migration hint.
	twice := wrapHubError(fmt.Errorf("outer: %w", wrapped))
	assert.Equal(t, 1, strings.Count(twice.Error(), "--broker"))
}

func TestRootProfileFlagHelp_MigrationGuidance(t *testing.T) {
	flag := rootCmd.PersistentFlags().Lookup("profile")
	require.NotNil(t, flag)
	assert.Contains(t, flag.Usage, "--broker")
	assert.Contains(t, flag.Usage, "legacy")
}
