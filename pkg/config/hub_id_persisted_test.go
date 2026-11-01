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

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadPersistedHubID reads the file PersistentHubID writes and never
// creates it.
func TestReadPersistedHubID(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	if got := ReadPersistedHubID(); got != "" {
		t.Fatalf("ReadPersistedHubID() = %q with no file, want empty", got)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, ".scion", hubIDFileName)); !os.IsNotExist(err) {
		t.Fatalf("ReadPersistedHubID created the hub-id file (stat err %v)", err)
	}
	id := PersistentHubID() // persists on first boot
	if got := ReadPersistedHubID(); got != id {
		t.Fatalf("ReadPersistedHubID() = %q, want the persisted %q", got, id)
	}
}
