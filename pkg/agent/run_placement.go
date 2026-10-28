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
	"errors"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Kubernetes placement defaults at Start (ptone/scion#5085). The agent
// runtime server resolves the placement for each start and hands it over in
// StartOptions; Start applies it over the agent's own kubernetes block here,
// and nowhere else.

// Fixed causes for a start whose placement was not resolved by the agent
// runtime server.
const (
	placementGlobalUnreadableCause = "the agent runtime's global settings could not be read; Kubernetes placement cannot be determined"
	placementLegacyGlobalCause     = "the agent runtime's global settings use the legacy format and mention Kubernetes placement keys; set schema_version: \"1\" so Kubernetes placement can be determined"
	placementUnresolvedCause       = "placement must be resolved by the agent runtime server"
)

// legacyPlacementNotice is logged when the agent's own config carries the
// kubernetes placement fields.
const legacyPlacementNotice = "kubernetes placement fields in the agent config are not the supported placement mechanism; configure placement in the agent runtime's settings"

// applyStartPlacement returns the kubernetes block a start passes to the
// runtime: a fresh copy of the agent's block agentK8s with the resolved
// placement applied (config.ApplyPlacement), and warnings for the caller.
//
// When the agent runtime server did not resolve placement
// (opts.PlacementResolved false), a Kubernetes start fails if the global
// settings cannot be read, or if they configure placement on a Kubernetes
// runtime entry or profile; otherwise it proceeds without placement. On a
// runtime other than Kubernetes a resolved placement is dropped with a
// warning.
func (m *AgentManager) applyStartPlacement(ctx context.Context, opts api.StartOptions, agentK8s *api.KubernetesConfig) (*api.KubernetesConfig, []string, error) {
	isKubernetes := m.Runtime != nil && m.Runtime.Name() == "kubernetes"
	if isKubernetes && hasLegacyPlacement(agentK8s) {
		slog.Info(legacyPlacementNotice, "agent", opts.Name)
	}
	if !opts.PlacementResolved {
		if isKubernetes {
			if err := checkUnresolvedPlacement(ctx, opts.Name); err != nil {
				return nil, nil, err
			}
		}
		return config.ApplyPlacement(agentK8s, nil)
	}
	rp := opts.ResolvedPlacement
	if !isKubernetes {
		if rp.InEffect() {
			runtimeName := ""
			if m.Runtime != nil {
				runtimeName = m.Runtime.Name()
			}
			slog.Warn("Start: Kubernetes placement applies only to the Kubernetes runtime; ignoring it",
				"agent", opts.Name, "runtime", runtimeName)
		}
		return config.ApplyPlacement(agentK8s, nil)
	}
	out, warnings, err := config.ApplyPlacement(agentK8s, rp)
	if err != nil {
		return nil, nil, err
	}
	if !rp.InEffect() {
		return out, nil, nil
	}
	slog.Debug("Start: applying Kubernetes placement", "agent", opts.Name,
		"nodeSelector", rp.NodeSelector, "tolerations", rp.Tolerations,
		"runtimeClassName", rp.RuntimeClassName, "sources", rp.Sources)
	if krt, ok := m.Runtime.(*runtime.KubernetesRuntime); ok && krt.GKEAutoDetected {
		for _, k := range config.MissingTolerationKeys(out.NodeSelector, out.Tolerations) {
			slog.Warn("Start: node selector key has no toleration with the same key; on GKE Autopilot, workload separation needs a NoSchedule toleration with the same key and value",
				"agent", opts.Name, "key", k)
		}
	}
	for _, w := range warnings {
		slog.Warn("Start: "+w, "agent", opts.Name)
	}
	return out, warnings, nil
}

// hasLegacyPlacement reports whether the agent's own kubernetes block sets
// any of the placement fields.
func hasLegacyPlacement(k *api.KubernetesConfig) bool {
	return k != nil && (len(k.NodeSelector) > 0 || len(k.Tolerations) > 0 || k.RuntimeClassName != "")
}

// checkUnresolvedPlacement is the check for a Kubernetes start whose
// placement the agent runtime server did not resolve: it fails when the
// global settings cannot be read (including a settings file that cannot be
// stat'd), when they are in the legacy format and mention a placement key,
// or when they configure placement on a Kubernetes runtime entry or on a
// profile of one (a profile whose runtime entry is not defined counts).
// It reads the request's settings view, so a flat instance request, which
// has no profile tier, is checked against its runtime entries; and a
// request of a flat instance with a Kubernetes runtime target always fails
// here, because that instance's placement (and its check of the agent's
// own fields) applies to every start, even with none configured.
func checkUnresolvedPlacement(ctx context.Context, agentName string) error {
	if p := config.FlatInstancePolicyFrom(ctx); p != nil && p.Target.Type == config.RuntimeTargetTypeKubernetes {
		slog.Warn("Start: the flat instance's placement was not resolved by the agent runtime server",
			"agent", agentName, "instance", p.InstanceKey)
		return errors.New(placementUnresolvedCause)
	}
	gs, _, err := config.LoadGlobalSettingsWithOverlayFor(ctx)
	if err != nil {
		slog.Warn("Start: global settings could not be read; refusing a Kubernetes start without resolved placement",
			"agent", agentName, "error", err)
		return errors.New(placementGlobalUnreadableCause)
	}
	// The loader treats a settings file it cannot stat as missing; placement
	// fails closed on it.
	if config.GlobalSettingsFileUnstatable() {
		slog.Warn("Start: the global settings file cannot be stat'd; refusing a Kubernetes start without resolved placement",
			"agent", agentName)
		return errors.New(placementGlobalUnreadableCause)
	}
	if config.GlobalSettingsIsLegacyFormat() && config.GlobalSettingsMentionPlacementKeys() {
		slog.Warn("Start: legacy-format global settings mention a placement key; refusing a Kubernetes start without resolved placement",
			"agent", agentName)
		return errors.New(placementLegacyGlobalCause)
	}
	if gs == nil {
		return nil
	}
	for name, rt := range gs.Runtimes {
		if config.IsKubernetesRuntimeEntry(name, rt) && config.RuntimeEntryHasPlacement(rt) {
			slog.Warn("Start: placement is configured but was not resolved by the agent runtime server",
				"agent", agentName, "runtime_entry", name)
			return errors.New(placementUnresolvedCause)
		}
	}
	for name, p := range gs.Profiles {
		if !config.ProfileHasPlacement(p) {
			continue
		}
		if rt, ok := gs.Runtimes[p.Runtime]; !ok || config.IsKubernetesRuntimeEntry(p.Runtime, rt) {
			slog.Warn("Start: placement is configured but was not resolved by the agent runtime server",
				"agent", agentName, "profile", name)
			return errors.New(placementUnresolvedCause)
		}
	}
	return nil
}
