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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// applyClearedFields applies the hub's cleared paths to merged, the agent's
// persisted config after the hub's inline config has been merged over it
// (ptone/scion#3906). It runs before the start reads that config
// (provisioning and the harness provisioners among them), so a cleared
// value is never handed to the agent again. Applying the same paths on
// every start gives the same result.
//
// What "inherited" means depends on how the start re-derives the config:
//   - A template resolved by name: GetAgent merges the template chain again
//     under the persisted config at every start, so a cleared key is simply
//     removed and the template's value (or, when the template sets none,
//     the start-time defaults) applies.
//   - A template hydrated from the content-addressed cache (agent-info.json
//     records a TemplateHash): GetAgent does not re-merge it, so the
//     template's values exist only in the persisted config. Each cleared
//     key is copied from that cached template's own config instead, a key
//     it leaves unset is removed. Only the cached template directory is
//     read, not a chain of base templates it was layered on at provision.
//   - A hydrated template this broker cannot load (no cache, an evicted
//     entry, an unreadable config): the cleared keys keep their current
//     value, so a clear never removes a limit; only env keys are removed.
//     The clear then takes effect at the agent's next reincarnation.
//
// Env keys (env.KEY) are always removed: a template's env is applied at
// start as a final fill, whichever way the template resolves.
func (s *Server) applyClearedFields(merged *api.ScionConfig, projectDir, agentName string, paths []string) {
	if len(paths) == 0 {
		return
	}
	inherit, hydrated, err := s.hydratedTemplateConfig(projectDir, agentName)
	if hydrated && err != nil {
		var kept, envOnly []string
		for _, p := range paths {
			if strings.HasPrefix(p, api.ClearedEnvPathPrefix) {
				envOnly = append(envOnly, p)
			} else if api.ClearableConfigPath(p) {
				kept = append(kept, p)
			}
		}
		if len(kept) > 0 {
			s.agentLifecycleLog.Warn("applyInlineConfigUpdate: cleared fields kept: the agent's cached template cannot be loaded to restore their inherited values",
				"agent", agentName, "fields", kept, "error", err)
		}
		paths = envOnly
	}
	if ignored := api.ApplyClearedFields(merged, paths, inherit); len(ignored) > 0 {
		s.agentLifecycleLog.Warn("applyInlineConfigUpdate: ignored cleared fields this broker does not apply", "agent", agentName, "fields", ignored)
	}
}

// hydratedTemplateConfig reports whether the agent's template was loaded
// from the content-addressed cache, as GetAgent decides it (a TemplateHash in
// agent-info.json, or a content hash recorded as the template name by older
// versions), and if so returns that cached template's config. err is set
// when the template is hydrated but its config cannot be loaded.
func (s *Server) hydratedTemplateConfig(projectDir, agentName string) (cfg *api.ScionConfig, hydrated bool, err error) {
	data, readErr := os.ReadFile(filepath.Join(config.GetAgentHomePath(projectDir, agentName), "agent-info.json"))
	if readErr != nil {
		return nil, false, nil
	}
	var info api.AgentInfo
	if json.Unmarshal(data, &info) != nil {
		return nil, false, nil
	}
	hash := info.TemplateHash
	if hash == "" && transfer.IsContentHash(info.Template) {
		hash = info.Template
	}
	if hash == "" {
		return nil, false, nil
	}
	if s.cache == nil {
		return nil, true, errors.New("no template cache")
	}
	dir, ok := s.cache.Get(hash)
	if !ok {
		return nil, true, errors.New("template not in the cache")
	}
	cfg, err = (&config.Template{Path: dir}).LoadConfig()
	if err != nil {
		return nil, true, fmt.Errorf("load cached template config: %w", err)
	}
	if cfg == nil {
		cfg = &api.ScionConfig{}
	}
	return cfg, true, nil
}

// workspaceMountTarget is the container path of the agent's workspace.
const workspaceMountTarget = "/workspace"

// dedupeVolumesByTarget collapses volumes that mount at the same target,
// keeping the first entry's position and the last entry's value, so a
// later entry (the hub's inline config, merged after the persisted config)
// replaces an earlier one for the same target instead of mounting twice.
// config.MergeScionConfig appends volumes, and the hub sends the agent's
// inline volumes on every start, so without this they grow with each start
// (ptone/scion#3906). Keying on the target, not the source, lets an
// override of a default mount replace it. Entries with no target are kept
// as they are.
//
// The workspace target is the exception: its first entry is kept and later
// ones are dropped. With no managed worktree, the start takes the agent's
// workspace from the first /workspace volume, the one provisioning wrote,
// and a volume added later must not move it.
func dedupeVolumesByTarget(vols []api.VolumeMount) []api.VolumeMount {
	if len(vols) < 2 {
		return vols
	}
	pos := make(map[string]int, len(vols))
	out := make([]api.VolumeMount, 0, len(vols))
	for _, vol := range vols {
		if vol.Target == "" {
			out = append(out, vol)
			continue
		}
		key := vol.Target
		if path.Clean(key) == workspaceMountTarget {
			key = workspaceMountTarget
		}
		if i, ok := pos[key]; ok {
			if key != workspaceMountTarget {
				out[i] = vol
			}
			continue
		}
		pos[key] = len(out)
		out = append(out, vol)
	}
	return out
}
