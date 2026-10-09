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
	"fmt"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// gitDepthEnvKey is the env var the in-container clone (sciontool init)
// reads for the clone depth: 0 means a full clone, N a depth-N clone.
const gitDepthEnvKey = "SCION_GIT_DEPTH"

// cloneDepthInput carries the clone_depth values in effect for a start.
type cloneDepthInput struct {
	// Template is clone_depth from the agent's merged template/agent
	// config. It wins over Profile.
	Template api.CloneDepth
	// Profile is clone_depth from the agent's settings profile.
	Profile api.CloneDepth
	// ProfileSource names the settings key Profile came from, for errors.
	ProfileSource string
}

// applyCloneDepth applies the effective clone_depth to a clone-per-agent
// start. The template value wins over the profile value; when neither is
// set, gc and env are left as they are, so the depth sent with the request
// (today a shallow depth-1 clone) still applies.
//
// When a value applies, it returns a copy of gc with Depth set ("full" is
// 0, N is N) and sets env[SCION_GIT_DEPTH] to match, so both the
// Kubernetes NFS provisioning init container (which reads Depth) and the
// in-container clone (which reads the env var) use it. gc itself is never
// modified. A nil gc (no per-agent clone) is returned as nil and env is
// left alone. An invalid value is an error that names where it was set.
func applyCloneDepth(gc *api.GitCloneConfig, env map[string]string, in cloneDepthInput) (*api.GitCloneConfig, error) {
	if gc == nil {
		return nil, nil
	}
	value, source := in.Template, "clone_depth in the agent or template config"
	if value == "" {
		value, source = in.Profile, "settings "+in.ProfileSource
	}
	depth, ok, err := value.GitDepth()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if !ok {
		return gc, nil
	}
	out := *gc
	out.Depth = &depth
	if env != nil {
		env[gitDepthEnvKey] = strconv.Itoa(depth)
	}
	return &out, nil
}
