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
	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// withoutRecord adapts a runtime-only resolver stub to the
// resolveAuxiliaryRuntime signature: every runtime it returns has the zero
// build record.
func withoutRecord(f func(projectPath, agentName, profileFlag string) scionrt.Runtime) func(string, string, string) (scionrt.Runtime, config.RuntimeBuildRecord) {
	return func(projectPath, agentName, profileFlag string) (scionrt.Runtime, config.RuntimeBuildRecord) {
		return f(projectPath, agentName, profileFlag), config.RuntimeBuildRecord{}
	}
}

// runtimeOnly is the reverse of withoutRecord: it drops the record, for a
// stub that wraps an existing resolveAuxiliaryRuntime.
func runtimeOnly(f func(projectPath, agentName, profileFlag string) (scionrt.Runtime, config.RuntimeBuildRecord)) func(string, string, string) scionrt.Runtime {
	return func(projectPath, agentName, profileFlag string) scionrt.Runtime {
		rt, _ := f(projectPath, agentName, profileFlag)
		return rt
	}
}

// managerAndType returns a resolution's manager and runtime type name, the
// two results resolveManagerForOpts returned before it returned a
// runtimeResolution.
func managerAndType(res runtimeResolution) (agent.Manager, string) {
	return res.Manager, res.RuntimeType
}
