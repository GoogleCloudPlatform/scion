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

package testutil

import (
	"slices"
	"testing"
)

// hubEnvKeys lists the environment variables that point the in-container
// Hub client at a Hub, authenticate it, or identify the agent to it. An
// agent container sets them ambiently. The values are spelled out here
// rather than imported so this package stays free of dependencies on the
// packages that use it.
var hubEnvKeys = []string{
	"SCION_HUB_ENDPOINT",
	"SCION_HUB_URL",
	"SCION_AUTH_TOKEN",
	"SCION_AGENT_ID",
	"SCION_AGENT_MODE",
	"SCION_TRANSPORT_TOKEN",
	"SCION_TRANSPORT_TOKEN_FILE",
}

// HubEnvKeys returns a fresh copy of the Hub client environment variable
// names that ScrubHubEnv clears, so callers can't change the shared list.
func HubEnvKeys() []string {
	return slices.Clone(hubEnvKeys)
}

// ScrubHubEnv clears every variable HubEnvKeys returns for the duration of the
// test, so a test running inside an agent container never talks to a real
// Hub and gives the same result as it would anywhere else.
func ScrubHubEnv(t testing.TB) {
	t.Helper()
	for _, key := range hubEnvKeys {
		t.Setenv(key, "")
	}
}
