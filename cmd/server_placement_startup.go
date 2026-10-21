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

package cmd

import (
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// checkPlacementSiblingsAtStartup is the agent runtime's startup check of
// the Kubernetes placement defaults (ptone/scion#5085): global Kubernetes
// runtime entries that resolve, on this host, to the same cluster and
// namespace must carry the same placement. A disagreement is logged as an
// error naming both entries, and startup continues: starts on that
// identity fail with a fixed cause, and the admin settings stay reachable
// to fix the value. It reads the overlay-inclusive global settings, so it
// runs after the settings overlay is installed, and only in a process that
// runs the agent runtime.
func checkPlacementSiblingsAtStartup(cfg *config.GlobalConfig, logf func(format string, args ...interface{})) {
	logPlacementSiblingsStartup(cfg != nil && cfg.RuntimeBroker.Enabled, loadGlobalSettingsForPlacement,
		config.ConfiguredRuntimeNames, hostKubeConnection, runtime.DefaultKubernetesNamespace(), logf)
}

func loadGlobalSettingsForPlacement() (*config.VersionedSettings, error) {
	gs, _, err := config.LoadGlobalSettingsWithOverlay()
	return gs, err
}

// hostKubeConnection is the current context and connection mode a
// Kubernetes client built on this host without an explicit context would
// have, read with the client's kubeconfig loading rules.
func hostKubeConnection() (string, config.KubeConnection, error) {
	ctx, mode, err := k8s.HostConnection(os.Getenv("KUBECONFIG"))
	return ctx, runtime.KubeConnectionOf(mode), err
}

// logPlacementSiblingsStartup does the check for checkPlacementSiblingsAtStartup
// with its inputs passed in. It never fails: it only logs.
func logPlacementSiblingsStartup(runtimeEnabled bool, load func() (*config.VersionedSettings, error),
	configuredNames func() (map[string]bool, bool),
	currentContext func() (string, config.KubeConnection, error), defaultNS string,
	logf func(format string, args ...interface{})) {
	if !runtimeEnabled {
		return
	}
	gs, err := load()
	if err != nil {
		if config.GlobalSettingsMentionPlacementKeys() {
			logf("Warning: Kubernetes placement: global settings failed to load (%v); Kubernetes starts fail until this is fixed", err)
		}
		return
	}
	if gs == nil || !placementConfigured(gs) {
		// Nothing to agree on: no identity resolution, no log lines.
		return
	}
	// Only configured runtime entries take part (entries that exist only in
	// the embedded defaults do not); all of them when that cannot be told.
	runtimes := gs.Runtimes
	if names, known := configuredNames(); known {
		runtimes = make(map[string]config.V1RuntimeConfig, len(names))
		for name, rt := range gs.Runtimes {
			if names[name] {
				runtimes[name] = rt
			}
		}
	}
	groups, warnings := config.IdentityGroups(runtimes, currentContext, defaultNS)
	for _, w := range warnings {
		logf("Warning: Kubernetes placement: %s", w)
	}
	for _, e := range config.ValidatePlacement(runtimes, gs.Profiles, groups) {
		logf("Error: Kubernetes placement: %s: %s", e.Path, e.Message)
	}
}

// placementConfigured reports whether any runtime entry or profile sets a
// Kubernetes placement field.
func placementConfigured(gs *config.VersionedSettings) bool {
	for _, rt := range gs.Runtimes {
		if len(rt.NodeSelector) > 0 || len(rt.Tolerations) > 0 || rt.RuntimeClassName != "" {
			return true
		}
	}
	for _, p := range gs.Profiles {
		if len(p.NodeSelector) > 0 || len(p.Tolerations) > 0 || p.RuntimeClassName != "" {
			return true
		}
	}
	return false
}
