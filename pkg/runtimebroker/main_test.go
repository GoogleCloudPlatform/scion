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
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
)

// TestMain isolates $HOME for the whole pkg/runtimebroker test binary.
// The broker resolves its global dir (~/.scion/projects,
// ~/.scion/project-configs, ~/.scion/runtime-broker-state, caches) through
// HOME, so without this, tests write into the real developer/agent HOME
// (ptone/scion#2445).
func TestMain(m *testing.M) {
	teardown := testutil.IsolateHome("scion-runtimebroker-test-home-*")
	code := m.Run()
	teardown()
	os.Exit(code)
}
