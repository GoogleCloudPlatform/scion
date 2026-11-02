// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// On Windows agent host state is refused with
// errors.ErrUnsupported, not protected by a path-based Lstat walk and a
// chmod. A read reports the error (not the zero state), an update writes
// nothing, and an existing file is left as it is. Control: the agent dir
// and file exist, so the refusal is not a missing-path result.
func TestWindowsHostState_Unsupported(t *testing.T) {
	a := newHostStateAgent(t)
	const existing = `{"runId":"run-1"}`
	if err := os.WriteFile(a.path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadHostState(a.root, a.name); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("ReadHostState = %v, want ErrUnsupported", err)
	} else if !strings.Contains(err.Error(), "Runtime Broker") {
		t.Errorf("ReadHostState error %q does not say where agent lifecycle runs", err)
	}
	called := false
	err := UpdateHostState(a.root, a.name, func(hs *HostState) error { called = true; hs.RunID = "run-2"; return nil })
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("UpdateHostState = %v, want ErrUnsupported", err)
	}
	if called {
		t.Error("UpdateHostState ran mutate on Windows")
	}
	if data, err := os.ReadFile(a.path); err != nil || string(data) != existing {
		t.Errorf("host state file after a refused update = %q, %v; want it unchanged", data, err)
	}
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("agent dir has %d entries after a refused update, want only %s", len(entries), HostStateFile)
	}
}
