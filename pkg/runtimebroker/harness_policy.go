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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// lookupHarnessConfigForPolicy resolves the harness-config that this
// dispatch will use, returning the entry needed by
// evaluateHarnessConfigPolicy. The resolution mirrors the logic in
// extractRequiredEnvKeys: prefer the on-disk harness-config dir (in the
// grove or global path), then fall back to any settings entry. ok is
// false when no harness-config was specified or could be found, which
// short-circuits the policy check (no policy applies).
func (s *Server) lookupHarnessConfigForPolicy(req CreateAgentRequest) (string, config.HarnessConfigEntry, bool) {
	var settings *config.VersionedSettings
	settingsPath := req.GrovePath
	if settingsPath == "" {
		if globalDir, err := config.GetGlobalDir(); err == nil {
			settingsPath = globalDir
		}
	}
	if settingsPath != "" {
		if vs, _, err := config.LoadEffectiveSettings(settingsPath); err == nil {
			settings = vs
		}
	}

	name := s.resolveHarnessConfigForEnvGather(req, settings)
	if name == "" {
		return "", config.HarnessConfigEntry{}, false
	}

	searchPath := req.GrovePath
	if searchPath == "" {
		searchPath = settingsPath
	}
	if searchPath != "" {
		if hcDir, err := config.FindHarnessConfigDir(name, searchPath); err == nil {
			return name, hcDir.Config, true
		}
	}
	if settings != nil {
		if hcfg, ok := settings.HarnessConfigs[name]; ok {
			return name, hcfg, true
		}
	}
	return name, config.HarnessConfigEntry{}, false
}

// harnessPolicyDecision describes the outcome of a per-dispatch policy check.
// Code is the API error code (matches errors.go constants); HTTPStatus is the
// HTTP status the broker should return; Message is the user-facing error
// string. An ok=true decision means the dispatch may proceed.
type harnessPolicyDecision struct {
	OK         bool
	Code       string
	HTTPStatus int
	Message    string
}

// evaluateHarnessConfigPolicy enforces broker-level dispatch policy on the
// resolved harness-config. Today it gates container-script provisioners
// behind ServerConfig.AllowContainerScriptHarnesses so brokers must opt in
// before staging Hub-distributed scripts that execute inside agent
// containers with access to projected secrets.
//
// Caller passes the resolved harness-config name (for logs/error text) and
// the parsed config entry. Returns a non-OK decision when the dispatch
// should be refused.
//
// The policy is intentionally minimal in v1 — only container-script type is
// gated. Future additions (e.g., trusted_harness_config_publishers) can
// extend this function without touching the dispatch path.
func (s *Server) evaluateHarnessConfigPolicy(harnessConfigName string, entry config.HarnessConfigEntry) harnessPolicyDecision {
	if entry.Provisioner == nil {
		return harnessPolicyDecision{OK: true}
	}
	if entry.Provisioner.Type != "container-script" {
		return harnessPolicyDecision{OK: true}
	}
	if s.config.AllowContainerScriptHarnesses {
		return harnessPolicyDecision{OK: true}
	}
	return harnessPolicyDecision{
		OK:         false,
		Code:       ErrCodeForbidden,
		HTTPStatus: 403,
		Message: fmt.Sprintf(
			"harness-config %q uses provisioner.type: container-script but this broker has allow_container_script_harnesses=false. Set broker.allow_container_script_harnesses=true (SCION_SERVER_BROKER_ALLOWCONTAINERSCRIPTHARNESSES=true) to enable.",
			harnessConfigName,
		),
	}
}
