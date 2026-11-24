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
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/firewallpolicy"
)

func TestStart_InitFirewallCopiedOnlyFromOpts(t *testing.T) {
	projectScionDir := setupTrustedHubEndpointTestProject(t, "agent-fw", "", "")
	wantPolicy, err := firewallpolicy.Normalize([]string{"pypi.org"}, true)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			capturedConfig = cfg
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:           "agent-fw",
		ProjectPath:    projectScionDir,
		BrokerMode:     true,
		NoAuth:         true,
		HubIssuedToken: fakeHubIssuedToken,
		InitFirewall:   wantPolicy,
		Env: map[string]string{
			firewallpolicy.EnvAllowDomains: "evil.example.com",
			firewallpolicy.EnvAllowSSH:     "true",
		},
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if !reflect.DeepEqual(capturedConfig.InitFirewall, wantPolicy) {
		t.Fatalf("RunConfig.InitFirewall = %#v, want %#v", capturedConfig.InitFirewall, wantPolicy)
	}
}
