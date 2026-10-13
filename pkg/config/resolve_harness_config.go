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

package config

import (
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// HarnessConfigResolution holds the result of resolving a harness-config name.
type HarnessConfigResolution struct {
	Name   string // resolved harness-config name
	Source string // for debug logging: "cli-flag", "template-default", etc.
}

// HarnessConfigInputs collects all the inputs needed to resolve a harness-config name.
type HarnessConfigInputs struct {
	CLIFlag      string           // --harness-config flag (highest priority)
	StoredConfig *api.ScionConfig // existing agent's stored config (resume path)
	TemplateCfg  *api.ScionConfig // merged template config
	Settings     *VersionedSettings
	ProfileName  string
}

// ResolveHarnessConfigName determines which harness-config to use for an agent.
// Resolution priority chain (first non-empty wins):
//  1. CLIFlag (--harness-config)
//  2. StoredConfig.HarnessConfig (existing agent's persisted config, resume path)
//  3. TemplateCfg.DefaultHarnessConfig (template's default_harness_config)
//  4. TemplateCfg.HarnessConfig (template's harness_config)
//  5. StoredConfig.Harness (legacy: harness name as fallback, resume only)
//  6. Profile's DefaultHarnessConfig (a flat instance's agent_defaults.default_harness_config)
//  7. Settings' top-level DefaultHarnessConfig
func ResolveHarnessConfigName(inputs HarnessConfigInputs) (*HarnessConfigResolution, error) {
	// 1. CLI flag
	if inputs.CLIFlag != "" {
		return resolved(inputs.CLIFlag, "cli-flag"), nil
	}

	// 2. Stored config (resume path — agent already provisioned)
	if inputs.StoredConfig != nil && inputs.StoredConfig.HarnessConfig != "" {
		return resolved(inputs.StoredConfig.HarnessConfig, "stored-config"), nil
	}

	// 3. Template's default_harness_config
	if inputs.TemplateCfg != nil && inputs.TemplateCfg.DefaultHarnessConfig != "" {
		return resolved(inputs.TemplateCfg.DefaultHarnessConfig, "template-default"), nil
	}

	// 4. Template's harness_config
	if inputs.TemplateCfg != nil && inputs.TemplateCfg.HarnessConfig != "" {
		return resolved(inputs.TemplateCfg.HarnessConfig, "template-harness-config"), nil
	}

	// 5. Stored harness name as fallback (resume path only)
	if inputs.StoredConfig != nil && inputs.StoredConfig.Harness != "" {
		return resolved(inputs.StoredConfig.Harness, "stored-harness-name"), nil
	}

	// 6. Profile's DefaultHarnessConfig (on a flat instance, the instance's
	// agent_defaults.default_harness_config; TierDefaultHarnessConfig)
	if name, source := inputs.Settings.TierDefaultHarnessConfig(inputs.ProfileName); name != "" {
		return resolved(name, source), nil
	}

	// 7. Settings' top-level default
	if inputs.Settings != nil && inputs.Settings.DefaultHarnessConfig != "" {
		return resolved(inputs.Settings.DefaultHarnessConfig, "settings-default"), nil
	}

	return nil, fmt.Errorf("no harness-config resolved. Specify --harness-config, set default_harness_config in the template, or set default_harness_config in settings")
}

func resolved(name, source string) *HarnessConfigResolution {
	util.Debugf("ResolveHarnessConfigName: name=%q source=%s", name, source)
	return &HarnessConfigResolution{Name: name, Source: source}
}
