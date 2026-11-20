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
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hostedguard"
)

// checkHostedHarnessConfig refuses, on a hosted Runtime Broker, volumes in
// a harness config written by project members: one bundled in a template,
// or one in the project's own harness-configs directory. The global
// harness-configs directory and settings harness_configs are operator
// content, and a hub-hydrated copy was checked by the Hub when it was
// stored; neither is checked. runtimeKind is the runtime that will run the
// agent, or "" when not known. It returns nil or a *hostedguard.RefusalError.
func checkHostedHarnessConfig(hosted bool, hcDir *config.HarnessConfigDir, runtimeKind string) error {
	if !hosted || hcDir == nil || len(hcDir.Config.Volumes) == 0 {
		return nil
	}
	if hcDir.Source != config.HarnessConfigSourceTemplateBundled && !hcDir.FromProjectTier() {
		return nil
	}
	return hostedguard.CheckMemberConfig(&api.ScionConfig{Volumes: hcDir.Config.Volumes},
		hostedguard.Origin{Kind: hostedguard.OriginHarnessConfig, Name: hcDir.Name}, runtimeKind)
}
