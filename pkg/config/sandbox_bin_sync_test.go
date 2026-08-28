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

// Package config_test (external test package) is used here to import
// pkg/runtime without creating a cycle.  pkg/runtime imports pkg/config,
// so the internal test package cannot import pkg/runtime.
package config_test

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestSandboxBinConstantSync_Task92 pins the sandbox binary path that
// pkg/config/init.go duplicates from pkg/runtime. Both packages need the
// same path: init.go uses it to detect the Cloud Run sandbox environment,
// and runtime uses it to launch sandboxes. If the two drift, init.go seeds
// a cloudrun-sandbox profile while GetRuntime picks a different runtime —
// a silent mismatch.
//
// Because init.go's constant is unexported (to avoid exporting an
// implementation detail from a lower-level package), this test asserts
// against the hardcoded literal that both copies share. If runtime's
// exported constant changes, this test fails; if init.go's copy changes,
// the InitMachine pin tests fail (they exercise isCloudRunSandboxEnvironment
// which uses the constant). Together they catch drift in either direction. (O5)
func TestSandboxBinConstantSync_Task92(t *testing.T) {
	// This is the path hardcoded in both pkg/config/init.go (unexported
	// defaultSandboxBin) and pkg/runtime/cloudrun_sandbox_runtime.go
	// (exported DefaultSandboxBin).
	const expectedPath = "/usr/local/gcp/bin/sandbox"

	if runtime.DefaultSandboxBin != expectedPath {
		t.Errorf("runtime.DefaultSandboxBin = %q, want %q — config/init.go has %q hardcoded; if this changed, update both",
			runtime.DefaultSandboxBin, expectedPath, expectedPath)
	}
}
