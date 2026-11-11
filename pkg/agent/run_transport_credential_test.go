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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStart_TransportCredentialReachesRunEachTime pins that every Start
// (fresh and resume) calls Runtime.Run with the transport credential from that
// call's options, so a re-dispatch with a freshly minted value reaches the
// runtime (and, on Kubernetes, the per-agent Secret) rather than a stale one.
func TestStart_TransportCredentialReachesRunEachTime(t *testing.T) {
	f := newSharedDirStorageRunFixture(t)
	f.writeGlobalSettings(t, "")

	var runEnvs [][]string
	var runResume []bool
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "kubernetes" },
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runEnvs = append(runEnvs, append([]string(nil), config.Env...))
			runResume = append(runResume, config.Resume)
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	for i, tc := range []struct {
		value  string
		resume bool
	}{
		{"first-value", false},
		{"second-value", true},
	} {
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "test-agent",
			ProjectPath: f.projectScionDir,
			NoAuth:      true,
			Resume:      tc.resume,
			Env: map[string]string{
				"SCION_AGENT_ID":                tc.value + "-agent",
				transportauth.EnvTransportToken: tc.value,
			},
		})
		require.NoError(t, err, "start %d", i)
		require.Len(t, runEnvs, i+1, "Start %d must call Runtime.Run", i)

		want := transportauth.EnvTransportToken + "=" + tc.value
		assert.Contains(t, runEnvs[i], want, "Run %d must receive this call's credential", i)
		for _, e := range runEnvs[i] {
			if e != want {
				assert.NotContains(t, e, transportauth.EnvTransportToken+"=", "Run %d must carry exactly one credential entry", i)
			}
		}
		assert.Equal(t, tc.resume, runResume[i])
	}
}
