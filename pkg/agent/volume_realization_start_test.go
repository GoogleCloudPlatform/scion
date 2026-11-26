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
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestStart_PassesConcreteRuntimeNameToVolumeCheck verifies that Start passes
// m.Runtime.Name() to runtime.CheckVolumeSourcesWithPolicy: a gke-shared-volume
// in a harness config is refused on a docker runtime (before Run is called) and
// accepted on a kubernetes runtime.
func TestStart_PassesConcreteRuntimeNameToVolumeCheck(t *testing.T) {
	hcBody := policyTestDecl + "volumes:\n  - type: gke-shared-volume\n    volume_name: shared-pvc\n    target: /mnt/gke\n"

	for _, tc := range []struct {
		runtimeName string
		wantRefused bool
	}{
		{runtimeName: "docker", wantRefused: true},
		{runtimeName: "kubernetes", wantRefused: false},
	} {
		t.Run(tc.runtimeName, func(t *testing.T) {
			e := newPolicyTestEnv(t)
			e.projectHC(t, "hc-gke", hcBody)

			var runCalls int
			mgr := NewManager(&runtime.MockRuntime{
				NameFunc: func() string { return tc.runtimeName },
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
					runCalls++
					return "mock-id", nil
				},
			})

			opts := api.StartOptions{
				Name:          "vol-rt-" + tc.runtimeName,
				ProjectPath:   e.scion,
				HarnessConfig: "hc-gke",
				NoAuth:        true,
				HostedMode:    false,
			}
			_, err := mgr.Start(context.Background(), opts)
			if tc.wantRefused {
				if !errors.Is(err, runtime.ErrUnsupportedVolumeType) {
					t.Fatalf("Start(%s) err = %v, want ErrUnsupportedVolumeType", tc.runtimeName, err)
				}
				if runCalls != 0 {
					t.Fatalf("Start(%s) called Run %d times after refusal, want 0", tc.runtimeName, runCalls)
				}
				return
			}
			if err != nil {
				t.Fatalf("Start(%s) err = %v, want nil", tc.runtimeName, err)
			}
			if runCalls != 1 {
				t.Fatalf("Start(%s) called Run %d times, want 1", tc.runtimeName, runCalls)
			}
		})
	}
}
