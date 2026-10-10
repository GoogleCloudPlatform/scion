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

package runtime

import (
	"testing"
)

// TestSandboxBinConstantSync_Task92 pins the sandbox binary path that
// pkg/config/init.go duplicates from pkg/runtime. Both packages need the
// same path: init.go uses it to detect the Cloud Run sandbox environment,
// and runtime uses it to launch sandboxes. If the two drift, init.go seeds
// a cloudrun-sandbox profile while GetRuntime picks a different runtime —
// a silent mismatch.
//
// This test asserts DefaultSandboxBin against the hardcoded literal.
// config's unexported defaultSandboxBin is NOT pinned by the InitMachine
// tests (they mock the sandboxBinExists seam, making them path-independent);
// TestDefaultSandboxBin_MatchesLiteral in pkg/config pins it against the
// same literal. Together the two tests catch drift in either direction.
// (O5)
//
// This test lives here rather than in pkg/config so that pkg/config's test
// build does not link pkg/runtime (and through it the Kubernetes client).
func TestSandboxBinConstantSync_Task92(t *testing.T) {
	// This is the path hardcoded in both pkg/config/init.go (unexported
	// defaultSandboxBin) and pkg/runtime/cloudrun_sandbox_runtime.go
	// (exported DefaultSandboxBin).
	const expectedPath = "/usr/local/gcp/bin/sandbox"

	if DefaultSandboxBin != expectedPath {
		t.Errorf("DefaultSandboxBin = %q, want %q — update both config and runtime if this changes",
			DefaultSandboxBin, expectedPath)
	}
}
