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

//go:build integration

package hubtestutil

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
)

// RunTestMain is the TestMain harness of pkg/hub and its area packages for
// the integration build. It starts the memory guard, isolates $HOME, clears
// the ambient GCP project env, installs the hermetic git ls-remote and
// sparse-checkout runners, sets up the ent test database
// (enttest.MainSetup), runs m, then undoes the setup in reverse order and
// returns the exit code for os.Exit. Unlike the default build's variant
// (runtestmain.go) it applies no leak guard.
//
// pkg/hub's integration TestMain (main_integration_test.go) delegates to it,
// so the steps live once.
func RunTestMain(m *testing.M) int {
	stopMemGuard := StartMemGuard()
	teardown := testutil.IsolateHome("scion-hub-test-home-*")
	ClearAmbientGCPProjectEnv()
	restoreLsRemote := InstallHermeticGitLsRemote()
	restoreSparseCheckout := InstallHermeticGitSparseCheckout()
	enttest.MainSetup()
	code := m.Run()
	enttest.MainTeardown()
	restoreSparseCheckout()
	restoreLsRemote()
	teardown()
	stopMemGuard()
	return code
}
