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

//go:build !integration

package hub

import (
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
)

// TestMain isolates $HOME for the whole (non-integration) pkg/hub test
// binary so tests keep scion state off the real developer/agent HOME (see
// ptone/scion#2417). The integration build has its own TestMain in
// main_integration_test.go, which applies the same isolation alongside the
// ent test database setup — Go allows only one TestMain per package per
// build, so the two are kept behind mutually exclusive build tags.
//
// It also starts the memory guard (mem_guard_helpers_test.go), which aborts
// the binary with goroutine stacks if process memory runs away, and clears
// the ambient GCP project env (clearAmbientGCPProjectEnv) so New() never
// builds real Cloud Logging clients from it (ptone/scion#3188).
//
// After the tests, the leak guard (leak_guard_helpers_test.go) fails the
// package if unclosed test stores or never-shut-down servers are still
// running (ptone/scion#3641).
func TestMain(m *testing.M) {
	stopMemGuard := startMemGuard()
	teardown := testutil.IsolateHome("scion-hub-test-home-*")
	clearAmbientGCPProjectEnv()
	code := m.Run()
	code = runLeakGuard(code)
	teardown()
	stopMemGuard()
	os.Exit(code)
}
