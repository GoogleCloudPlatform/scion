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

package hub

import (
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
)

// TestMain isolates $HOME (see testutil.IsolateHome) in addition to the
// existing ent test database setup, so integration-tagged pkg/hub tests
// keep scion state off the real developer/agent HOME (ptone/scion#2417),
// and starts the same memory guard as the non-integration TestMain.
func TestMain(m *testing.M) {
	stopMemGuard := startMemGuard()
	teardown := testutil.IsolateHome("scion-hub-test-home-*")
	clearAmbientGCPProjectEnv()
	restoreLsRemote := installHermeticGitLsRemote()
	enttest.MainSetup()
	code := m.Run()
	enttest.MainTeardown()
	restoreLsRemote()
	teardown()
	stopMemGuard()
	os.Exit(code)
}
