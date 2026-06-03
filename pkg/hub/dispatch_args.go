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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// StartDispatchArgs carries the resolved parameters for a cross-node agent
// start. All env/secret resolution happens on the ORIGINATOR (design §5.2),
// so secrets land in the DB args column rather than traveling over NOTIFY.
type StartDispatchArgs struct {
	Task            string            `json:"task,omitempty"`
	ResolvedEnv     map[string]string `json:"resolvedEnv,omitempty"`
	ResolvedSecrets []ResolvedSecret  `json:"resolvedSecrets,omitempty"`
	InlineConfig    *api.ScionConfig  `json:"inlineConfig,omitempty"`
	SharedDirs      []api.SharedDir   `json:"sharedDirs,omitempty"`
	SharedWorkspace bool              `json:"sharedWorkspace,omitempty"`
	ProjectPath     string            `json:"projectPath,omitempty"`
	ProjectSlug     string            `json:"projectSlug,omitempty"`
	HarnessConfig   string            `json:"harnessConfig,omitempty"`
}

// RestartDispatchArgs carries the resolved env for a cross-node agent restart.
// The restart path re-resolves auth tokens and identity vars so the restarted
// container has valid Hub credentials.
type RestartDispatchArgs struct {
	ResolvedEnv map[string]string `json:"resolvedEnv,omitempty"`
}

// StopDispatchArgs is intentionally empty — a stop needs no additional params
// beyond what the dispatch row already carries (agentSlug, projectID).
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

// UnmarshalRestartArgs deserializes restart dispatch args.
func UnmarshalRestartArgs(raw string) (*RestartDispatchArgs, error) {
	var a RestartDispatchArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil, err
	}
	return &a, nil
}
