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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func getTestHarnesses() []api.Harness {
	return []api.Harness{
		&harness.Generic{},
	}
}

// mockRuntimeForTest overrides runtime detection so InitMachine/InitProject
// succeed without a real container runtime installed.
func mockRuntimeForTest(t *testing.T) {
	t.Helper()
	restore := config.OverrideRuntimeDetection(
		func(file string) (string, error) { return "/usr/bin/" + file, nil },
		func(binary string, args []string) error { return nil },
	)
	t.Cleanup(restore)
}
