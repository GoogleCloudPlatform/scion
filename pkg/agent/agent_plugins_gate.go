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
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agentplugin"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/resources"
)

// agentPluginsEnabled reports whether hub.agent_plugins is on for this
// provisioning/start. In broker mode only the hub decides (dispatch
// experiments); the broker host's settings are never consulted, so a broker
// operator cannot enable it for a hub that has it off. Outside broker mode
// (solo/local CLI) the effective settings' local_experiments decide.
func agentPluginsEnabled(ctx context.Context, settings *config.VersionedSettings) bool {
	if api.IsBrokerModeFromContext(ctx) {
		return api.HubAgentDefaultsFromContext(ctx).ExperimentEnabled(experiments.AgentPlugins)
	}
	return settings != nil && slices.Contains(settings.LocalExperiments, experiments.AgentPlugins)
}

// hasAgentPlugins reports whether any template in chain declares
// agent_plugins.
func hasAgentPlugins(chain []*config.Template) bool {
	for _, tpl := range chain {
		if tpl == nil {
			continue
		}
		cfg, err := tpl.LoadConfig()
		if err == nil && cfg != nil && len(cfg.AgentPlugins) > 0 {
			return true
		}
	}
	return false
}

// agentPluginTemplateSources reloads each template's own config so every
// agent_plugins entry resolves against the directory of the template that
// declares it.
func agentPluginTemplateSources(chain []*config.Template) []agentplugin.TemplateSource {
	var out []agentplugin.TemplateSource
	for _, tpl := range chain {
		if tpl == nil {
			continue
		}
		cfg, err := tpl.LoadConfig()
		if err != nil || cfg == nil || len(cfg.AgentPlugins) == 0 {
			continue
		}
		out = append(out, agentplugin.TemplateSource{
			Name: templateRef(tpl),
			Dir:  tpl.Path,
			Refs: slices.Clone(cfg.AgentPlugins),
		})
	}
	return out
}

// warnInlineAgentPlugins reports agent_plugins entries of the inline config
// that no template in the chain declares: Phase 1 resolves only
// template-relative paths, each against its own template directory.
func warnInlineAgentPlugins(ctx context.Context, inline *api.ScionConfig, sources []agentplugin.TemplateSource) {
	if inline == nil {
		return
	}
	declared := map[string]bool{}
	for _, src := range sources {
		for _, ref := range src.Refs {
			declared[agentPluginPathKey(ref.Path)] = true
		}
	}
	for _, ref := range inline.AgentPlugins {
		if declared[agentPluginPathKey(ref.Path)] {
			continue
		}
		addStartWarning(ctx, fmt.Sprintf("agent_plugins: entry %q ignored: only template-relative paths are supported", ref.Path))
	}
}

func agentPluginPathKey(p string) string {
	return filepath.Clean(strings.TrimPrefix(p, "./"))
}

// resolvedSkillNames lists the skill directory names installed from the
// skill bank, as recorded in rec.
func resolvedSkillNames(rec *SkillResolutionRecord) []string {
	if rec == nil {
		return nil
	}
	var out []string
	for _, e := range rec.Skills {
		switch {
		case e.InstalledPath != "":
			out = append(out, filepath.Base(e.InstalledPath))
		case e.As != "":
			out = append(out, e.As)
		default:
			out = append(out, e.Name)
		}
	}
	return out
}

// platformSkillNames lists the top-level skill directory names of the
// embedded platform skills.
func platformSkillNames() []string {
	entries, err := fs.ReadDir(resources.PlatformSkillsFS(), ".")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// reservedSkillNames merges the skill directory names this provisioning
// installs into the harness skills dir.
func reservedSkillNames(lists ...[]string) map[string]bool {
	out := map[string]bool{}
	for _, l := range lists {
		for _, n := range l {
			if n != "" {
				out[n] = true
			}
		}
	}
	return out
}

// agentPluginsCapabilityMissing reports whether h does not support Agent
// Plugins: an empty Support (a harness-config that predates the capability)
// counts as no.
func agentPluginsCapabilityMissing(h api.Harness) bool {
	s := h.AdvancedCapabilities().AgentPlugins.Support
	return s == "" || s == api.SupportNo
}

// unsupportedAgentPluginsMessage is the start warning for a harness that
// cannot install staged plugins.
func unsupportedAgentPluginsMessage(harnessName string, h api.Harness) string {
	c := h.AdvancedCapabilities().AgentPlugins
	if c.Support == "" {
		return fmt.Sprintf("agent_plugins: harness-config %q predates Agent Plugins support; plugins staged but not installed (reinstall it with scion harness-config install --force)", harnessName)
	}
	reason := c.Reason
	if reason == "" {
		reason = "no native plugin support"
	}
	return fmt.Sprintf("agent_plugins: harness %q does not support Agent Plugins; plugins staged but not installed (%s)", harnessName, reason)
}

// startAgentPluginsWarning is the start warning about agent plugins for a
// start of a container-script harness, or "": recordHas reports a staged
// plugin index in the control-plane record, includePlugins the gate for this
// start, and h the harness the start resolved (which may differ from the one
// provisioned, after a --harness-config switch).
func startAgentPluginsWarning(recordHas, includePlugins bool, harnessName string, h api.Harness) string {
	switch {
	case !recordHas:
		return ""
	case !includePlugins:
		return "agent_plugins: ignored: experiment hub.agent_plugins is off; plugins Scion installed earlier, if any, are removed"
	case agentPluginsCapabilityMissing(h):
		return unsupportedAgentPluginsMessage(harnessName, h)
	}
	return ""
}

// isContainerScriptHarness reports whether h installs from staged inputs.
func isContainerScriptHarness(h api.Harness) bool {
	_, ok := h.(*harness.ContainerScriptHarness)
	return ok
}
