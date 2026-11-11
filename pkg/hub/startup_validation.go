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
	"context"
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ValidateStartupDefaults checks that the hub's operational agent-creation
// defaults (agent_defaults.default_template and .default_harness_config)
// actually resolve to a registered resource, and logs a clear warning when
// they do not.
//
// ptone/scion#1316 fault 3: nothing previously validated a deployment's own
// defaults before the first agent-create. A hub could be "born broken" — a
// default naming a resource nothing had registered — and report itself
// healthy right up until an operator's first create request failed with a
// broker error naming a resource they never typed. Call this once after
// bundled resources are seeded and operational settings are loaded (the end
// of initHubServer), so the operator sees the warning at boot instead of at
// the first create.
//
// Deliberately advisory: an unresolved default degrades a create rather than
// makes the hub unusable — the per-request path (applyHubAgentDefaults /
// populateAgentConfig) already logs and proceeds without the default, and
// the broker may still resolve the name from its own local search path — so
// startup does not fail. Both checks use global scope only, matching the
// scope a hub-wide default is defined and applied in; there is no project
// context to prefer at startup.
func (s *Server) ValidateStartupDefaults(ctx context.Context) {
	d := s.hubAgentDefaults()

	if name := d.DefaultTemplate; name != "" {
		tpl, err := s.resolveTemplate(ctx, name, "")
		switch {
		case err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, config.ErrTemplateNotFound):
			s.agentLifecycleLog.Warn(
				"startup validation: failed to look up hub agent_defaults.default_template; "+
					"agent creates relying on this default may fail unexpectedly",
				"template", name, "error", err)
		case tpl == nil:
			s.agentLifecycleLog.Warn(
				"startup validation: hub agent_defaults.default_template does not resolve to "+
					"a registered template; agents created with no explicit template will be "+
					"created with none. Fix or clear default_template in the hub agent_defaults settings",
				"template", name)
		}
	}

	if name := d.DefaultHarnessConfig; name != "" {
		hc, err := s.store.GetHarnessConfigBySlug(ctx, name, store.HarnessConfigScopeGlobal, "")
		switch {
		case err != nil && !errors.Is(err, store.ErrNotFound):
			s.agentLifecycleLog.Warn(
				"startup validation: failed to look up hub agent_defaults.default_harness_config; "+
					"agent creates relying on this default may fail unexpectedly",
				"harness_config", name, "error", err)
		case hc == nil:
			s.agentLifecycleLog.Warn(
				"startup validation: hub agent_defaults.default_harness_config does not resolve "+
					"to a registered harness-config; agent creates that rely on this default will "+
					"dispatch with no ID/hash for the broker to hydrate, and will fail with a "+
					"\"harness-config not found\" error if the broker cannot resolve the name from "+
					"its own local search path either. Fix or clear default_harness_config in the "+
					"hub agent_defaults settings",
				"harness_config", name)
		}
	}
}
