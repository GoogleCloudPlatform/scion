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
)

// StartDispatchArgs carries the parameters for a cross-node agent start.
// Only fields that the owner's DispatchAgentStart cannot re-derive are
// included. Env/secret resolution is performed by the OWNER via
// DispatchAgentStart (all hub instances share the same store + secret
// backend), so resolved env/secrets are NOT serialized here.
type StartDispatchArgs struct {
	Task string `json:"task,omitempty"`
}

// RestartDispatchArgs is intentionally empty — the owner's
// DispatchAgentRestart re-resolves auth tokens and identity vars from the
// shared store on the owning node.
type RestartDispatchArgs struct{}

// StopDispatchArgs is intentionally empty — a stop needs no additional params
// beyond what the dispatch row already carries (agentID, projectID).
type StopDispatchArgs struct{}

// MarshalDispatchArgs serializes a dispatch args struct to JSON for storage in
// broker_dispatch.args.
func MarshalDispatchArgs(v interface{}) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// UnmarshalStartArgs deserializes start dispatch args from the broker_dispatch row.
func UnmarshalStartArgs(raw string) (*StartDispatchArgs, error) {
	var a StartDispatchArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil, err
	}
	return &a, nil
}
