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

package runtime

import (
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// launchChildLabels is the one label mapping for a launch's per-agent child
// objects (Secrets, SecretProviderClasses): scion.agent=<agentName>, then
// every scion.* label of the run copied over it (the owner, project and run
// labels among them), plus the run's immutable agent ID label
// (api.LabelAgentID) copied from the run labels, never derived. A flat
// Runtime Broker instance recovers an orphaned child from these labels alone
// (ptone/scion#3274).
func launchChildLabels(agentName string, runLabels map[string]string) map[string]string {
	out := map[string]string{"scion.agent": agentName}
	for k, v := range runLabels {
		if strings.HasPrefix(k, "scion.") {
			out[k] = v
		}
	}
	if id := runLabels[api.LabelAgentID]; id != "" {
		out[api.LabelAgentID] = id
	}
	return out
}
