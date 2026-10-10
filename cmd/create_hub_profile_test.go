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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3933: in Hub mode, scion create sends -p and --broker to the
// Hub, as scion start does, so an explicit flag wins over the project and
// Hub defaults. Without them the request names neither, and the Hub applies
// its defaults.
func TestCreateAgentViaHub_SendsProfileAndBroker(t *testing.T) {
	const projectID, agentName = "proj-create-profile", "create-profile-agent"

	for _, tc := range []struct {
		name, profile, broker string
	}{
		{"profile and broker", "gke", "gke-broker"},
		{"profile only", "gke", ""},
		{"neither", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetHubStartGlobals(t)
			origProfile := profile
			t.Cleanup(func() { profile = origProfile })
			profile, runtimeBrokerID = tc.profile, tc.broker

			stub := newHubStartStub(t, projectID, agentName, "")
			var err error
			captureStdIO(t, func() {
				err = createAgentViaHub(stub.hubCtx(t, projectID), agentName, "")
			})
			require.NoError(t, err)
			require.Equal(t, 1, stub.createCalls)

			assertBodyField(t, stub.createBody, "profile", tc.profile)
			assertBodyField(t, stub.createBody, "runtimeBrokerId", tc.broker)
		})
	}
}

// assertBodyField checks that key carries want, or is omitted when want is "".
func assertBodyField(t *testing.T, body map[string]interface{}, key, want string) {
	t.Helper()
	v, present := body[key]
	if want == "" {
		assert.False(t, present, "%s must be omitted when not set", key)
		return
	}
	assert.Equal(t, want, v, key)
}
