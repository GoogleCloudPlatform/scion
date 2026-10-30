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
	"context"
	"os"
	"path/filepath"
	"testing"
)

// newWorkaroundTestRuntime creates a CloudRunSandboxRuntime wired to a mock
// shell-script binary. The deleteTimeout is set short so tests complete
// quickly.
func newWorkaroundTestRuntime(t *testing.T, bin string) *CloudRunSandboxRuntime {
	t.Helper()
	stateFile := filepath.Join(t.TempDir(), "state.json")
	return &CloudRunSandboxRuntime{
		bin:          bin,
		state:        newSandboxStateStore(stateFile),
		rootDir:      t.TempDir(),
		watchCancels: make(map[string]context.CancelFunc),
	}
}

// writeMockBin writes a shell script as a mock sandbox binary and returns
// the path.
func writeMockBin(t *testing.T, script string) string {
	t.Helper()
	tmpDir := t.TempDir()
	mockBin := filepath.Join(tmpDir, "sandbox")
	if err := os.WriteFile(mockBin, []byte("#!/bin/sh\n"+script+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return mockBin
}
