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

package hub

import (
	"encoding/json"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// retiredAsyncAgentLaunchInBody reports whether the raw admin server-config
// body has server.hub.async_agent_launch, whatever its value (including
// null).
func retiredAsyncAgentLaunchInBody(rawBody []byte) bool {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &top); err != nil {
		return false
	}
	var server map[string]json.RawMessage
	if err := json.Unmarshal(top["server"], &server); err != nil {
		return false
	}
	var hub map[string]json.RawMessage
	if err := json.Unmarshal(server["hub"], &hub); err != nil {
		return false
	}
	_, ok := hub["async_agent_launch"]
	return ok
}

// rejectRetiredAsyncAgentLaunch writes a 422 and returns true when the raw
// admin server-config body sets the retired server.hub.async_agent_launch.
// Both PUT handlers call it: their typed decode would otherwise drop the key
// silently, and an admin would believe async create was on. Async create is
// the hub.async_agent_launch experiment (ptone/scion#4991).
func rejectRetiredAsyncAgentLaunch(w http.ResponseWriter, rawBody []byte) bool {
	if !retiredAsyncAgentLaunchInBody(rawBody) {
		return false
	}
	writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationError, config.RetiredAsyncAgentLaunchWarning, nil)
	return true
}
