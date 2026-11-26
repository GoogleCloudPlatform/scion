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
	"errors"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// agentPolicyErrorFrom returns the operator agent_policy refusal
// (*agent.AgentPolicyError) carried by err, wrapped or not.
func agentPolicyErrorFrom(err error) (*agent.AgentPolicyError, bool) {
	var pe *agent.AgentPolicyError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// agentPolicyRefusedStatus is the HTTP status a start refused by the broker's
// operator agent_policy answers with.
const agentPolicyRefusedStatus = http.StatusBadRequest

// writeAgentPolicyRefused answers a start refused by the broker's operator
// agent_policy with 400 agent_policy_refused and the error's bounded public
// message. The caller logs the full error.
func writeAgentPolicyRefused(w http.ResponseWriter, pe *agent.AgentPolicyError, op string, details map[string]interface{}) {
	writeError(w, agentPolicyRefusedStatus, api.LaunchCodeAgentPolicyRefused, failedOpText(op)+": "+pe.PublicMessage(), details)
}
