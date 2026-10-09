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

package api

// Runtime broker error codes the hub matches on to relay a specific broker
// refusal instead of folding it into a generic dispatch failure. They live
// here so the broker (which writes them) and the hub (which reads them)
// share one definition without the hub importing pkg/runtimebroker.
const (
	// BrokerErrCodeSkillResolution marks a create whose required skills
	// could not be resolved.
	BrokerErrCodeSkillResolution = "skill_resolution_failed"

	// BrokerErrCodeWorkspaceStorageUnconfigured marks a create whose
	// workspace was uploaded to bucket storage (workspaceStoragePath set)
	// when neither the request nor the broker names the bucket to download
	// it from (ptone/scion#3422).
	BrokerErrCodeWorkspaceStorageUnconfigured = "workspace_storage_unconfigured"

	// BrokerErrCodeHarnessConfigUnusable marks a dispatch whose
	// harness-config provisioner cannot run (422). It is a configuration
	// error the caller must fix (ptone/scion#3132).
	BrokerErrCodeHarnessConfigUnusable = "harness_config_unusable"
)
