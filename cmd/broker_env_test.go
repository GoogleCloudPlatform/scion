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
	"context"
	"testing"
)

// isolateJoinEnv clears the environment the join and mint commands read, so
// values exported by the surrounding container do not leak in.
func isolateJoinEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		envBrokerJoinToken, envBrokerID, "SCION_HUB_ENDPOINT", "SCION_HUB_URL",
		"SCION_AUTH_TOKEN", "SCION_HUB_TOKEN", "SCION_DEV_TOKEN", "SCION_DEV_TOKEN_FILE",
		"SCION_TRANSPORT_MODE", "SCION_TRANSPORT_AUDIENCE",
	} {
		t.Setenv(k, "")
	}
	savedProjectPath, savedGlobal, savedHubEndpoint := projectPath, globalMode, hubEndpoint
	savedBrokerID, savedHubName := brokerJoinBrokerID, brokerHubName
	savedTokenFile, savedForce := brokerJoinTokenFile, brokerJoinForce
	savedTTL, savedJSON := hubBrokersJoinTokenTTL, hubBrokersJoinTokenJSON
	t.Cleanup(func() {
		projectPath, globalMode, hubEndpoint = savedProjectPath, savedGlobal, savedHubEndpoint
		brokerJoinBrokerID, brokerHubName = savedBrokerID, savedHubName
		brokerJoinTokenFile, brokerJoinForce = savedTokenFile, savedForce
		brokerJoinCmd.SetIn(nil)
		hubBrokersJoinTokenTTL, hubBrokersJoinTokenJSON = savedTTL, savedJSON
	})
	// cobra sets a context when the command is executed; these tests call
	// the run functions directly.
	brokerJoinCmd.SetContext(context.Background())
	hubBrokersJoinTokenCreateCmd.SetContext(context.Background())
	globalMode, hubEndpoint = false, ""
	brokerJoinBrokerID, brokerHubName = "", ""
	brokerJoinTokenFile, brokerJoinForce = "", false
	hubBrokersJoinTokenTTL, hubBrokersJoinTokenJSON = 0, false
}
