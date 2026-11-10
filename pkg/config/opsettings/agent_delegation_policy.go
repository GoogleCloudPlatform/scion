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

package opsettings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
)

// AgentDelegationPolicySectionName is the name of the operational settings
// section that narrows the hub agent-delegation policy
// (.design/agent-delegation.md §10, §13).
const AgentDelegationPolicySectionName = "agent_delegation_policy"

// Boundary kinds the agent_delegation_policy section narrows. They match
// the grant boundary kinds.
const (
	AgentDelegationBoundaryProject = "project"
	AgentDelegationBoundaryHub     = "hub"
)

// AgentDelegationPolicySettings narrows the hub agent-delegation policy.
// DB-only (runtime state), no settings.yaml representation. Absent row = no
// narrowing.
//
// The section can only remove permissions from the code-owned policy
// (permissions.AgentDelegableRegistry), per boundary kind. It can never add
// one: a permission ID listed here that the policy does not hold has no
// effect. Changes apply at the next exchange and on every use.
type AgentDelegationPolicySettings struct {
	// Remove maps a boundary kind ("project" or "hub") to the permission
	// IDs removed from the policy for grants of that kind.
	Remove map[string][]string `json:"remove,omitempty"`
}

// Removes reports whether the section removes permissionID from the policy
// for grants of boundary kind kind.
func (s AgentDelegationPolicySettings) Removes(kind, permissionID string) bool {
	return slices.Contains(s.Remove[kind], permissionID)
}

// ParseAgentDelegationPolicyDoc parses a stored agent_delegation_policy
// document. Unknown fields, unknown boundary kinds and empty permission IDs
// are errors, so a document the code cannot fully read is never taken as
// "no narrowing".
func ParseAgentDelegationPolicyDoc(raw json.RawMessage) (AgentDelegationPolicySettings, error) {
	var doc AgentDelegationPolicySettings
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return AgentDelegationPolicySettings{}, fmt.Errorf("agent_delegation_policy settings: %w", err)
	}
	if dec.More() {
		return AgentDelegationPolicySettings{}, fmt.Errorf("agent_delegation_policy settings: trailing data")
	}
	for kind, ids := range doc.Remove {
		if kind != AgentDelegationBoundaryProject && kind != AgentDelegationBoundaryHub {
			return AgentDelegationPolicySettings{}, fmt.Errorf("agent_delegation_policy settings: unknown boundary kind %q", kind)
		}
		for _, id := range ids {
			if id == "" {
				return AgentDelegationPolicySettings{}, fmt.Errorf("agent_delegation_policy settings: empty permission ID")
			}
		}
	}
	return doc, nil
}

// agentDelegationPolicySchema is the hand-written JSON schema of the
// agent_delegation_policy section (runtime state, no $defs in
// settings-v1.schema.json).
func agentDelegationPolicySchema() map[string]interface{} {
	permissionList := map[string]interface{}{
		"type":  "array",
		"items": map[string]interface{}{"type": "string", "minLength": 1},
	}
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"remove": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					AgentDelegationBoundaryProject: permissionList,
					AgentDelegationBoundaryHub:     permissionList,
				},
				"additionalProperties": false,
			},
		},
		"additionalProperties": false,
	}
}
