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
	"context"
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
// The inherited value is what provisioning derived from the agent's
// template chain (see clearedFieldsInheritance):
//   - A template resolved by name: GetAgent merges the same chain again
//     under the persisted config at every start and restart, so a cleared
//     key is removed and the chain's value (or, when the chain sets none,
//     the start-time defaults) applies, following later template changes.
//   - A template hydrated from the content-addressed cache: GetAgent does
//     not re-merge it, so each cleared key is copied from the resolved
//     chain instead, a key the chain leaves unset is removed.
//   - A chain that does not resolve (a cache miss, a template name that no
//     longer resolves, an unreadable layer): the cleared keys keep their
//     current value, so a clear never removes a limit, and the clear takes
//     effect at the agent's next reincarnation.
//
// Env keys (env.KEY) are always removed: a template's env is applied at
// start as a final fill, whichever way the template resolves.
func (s *Server) applyClearedFields(merged *api.ScionConfig, projectPath, projectDir, agentName string, paths []string) {
	if len(paths) == 0 {
		return
	}
	inherit, err := s.clearedFieldsInheritance(projectPath, projectDir, agentName)
	if err != nil {
		var kept, applied []string
		for _, p := range paths {
			if api.ClearableConfigPath(p) && !strings.HasPrefix(p, api.ClearedEnvPathPrefix) {
				kept = append(kept, p)
				continue
			}
			// Env keys are applied; paths ApplyClearedFields rejects still
			// reach it, so they are reported as ignored.
			applied = append(applied, p)
		}
		if len(kept) > 0 {
			s.agentLifecycleLog.Warn("applyInlineConfigUpdate: cleared fields kept: the agent's template chain cannot be resolved to restore their inherited values",
				"agent", agentName, "fields", kept, "error", err)
		}
		paths = applied
	}
	if ignored := api.ApplyClearedFields(merged, paths, inherit); len(ignored) > 0 {
		s.agentLifecycleLog.Warn("applyInlineConfigUpdate: ignored cleared fields this broker does not apply", "agent", agentName, "fields", ignored)
	}
}

// clearedFieldsInheritance resolves the template chain the agent was
// provisioned from, as provisioning resolves it: the template agent-info.json
// names (or the settings' default template when it names none), or, for a
// template hydrated from the content-addressed cache (a TemplateHash, or a
// content hash recorded as the name by older versions), the cached
// directory; GetTemplateChainInProject layers the project or global
// default template first. Each layer contributes its job keys only
// (config.StripTemplateEnvironmentKeys, without env), as at provision, so a
// template's resources are never introduced.
//
// It returns the merged chain for a hydrated template, whose values the
// caller copies, and nil for a template resolved by name, whose cleared
// keys the caller removes because GetAgent re-merges the chain at start.
// err is set when the chain does not resolve.
func (s *Server) clearedFieldsInheritance(projectPath, projectDir, agentName string) (*api.ScionConfig, error) {
	templateRef := "default"
	if vs, _, _ := config.LoadEffectiveSettingsFor(context.Background(), projectDir); vs != nil && vs.DefaultTemplate != "" {
		templateRef = vs.DefaultTemplate
	}
	var hash string
	if data, err := os.ReadFile(filepath.Join(config.GetAgentHomePath(projectDir, agentName), "agent-info.json")); err == nil {
		var info api.AgentInfo
		if json.Unmarshal(data, &info) == nil {
			hash = info.TemplateHash
			if transfer.IsContentHash(info.Template) {
				if hash == "" {
					hash = info.Template
				}
			} else if info.Template != "" {
				templateRef = info.Template
			}
		}
	}
	hydrated := hash != ""
	if hydrated {
		if s.cache == nil {
			return nil, errors.New("no template cache")
		}
		dir, ok := s.cache.Get(hash)
		if !ok {
			return nil, errors.New("template not in the cache")
		}
		templateRef = dir
	}
	chain, err := config.GetTemplateChainInProject(templateRef, projectPath)
	if err != nil {
		return nil, fmt.Errorf("resolve template chain: %w", err)
	}
	inherited := &api.ScionConfig{}
	for _, tpl := range chain {
		tplCfg, err := tpl.LoadConfig()
		if err != nil {
			return nil, fmt.Errorf("load template config: %w", err)
		}
		stripped, _ := config.StripTemplateEnvironmentKeys(tplCfg)
		if stripped == nil {
			continue
		}
		job := *stripped
		job.Env = nil
		inherited = config.MergeScionConfig(inherited, &job)
	}
	if !hydrated {
		return nil, nil
	}
	return inherited, nil
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
