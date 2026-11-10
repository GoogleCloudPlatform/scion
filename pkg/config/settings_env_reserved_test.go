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
	"runtime"
	"strings"
	"testing"
)

// agentRefusedSampleNames covers the server prefix, the legacy session secret
// alias and both host credential aliases.
var agentRefusedSampleNames = []string{
	"SCION_SERVER_SESSION_SECRET",
	"SESSION_SECRET",
	"SCION_SERVER_HUB_CONDUIT_PEERSECRET",
	"SCION_SERVER_HUB_CONDUIT_PEER_SECRET",
	"SCION_DEV_TOKEN",
	"SCION_AUTH_TOKEN",
}

// TestGitTracked_ChildEnvOmitsRefusedNames runs gitTracked against a fake git
// that records its env, and checks the child did not inherit server secret or
// host credential names while ordinary variables still pass.
func TestGitTracked_ChildEnvOmitsRefusedNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake git script requires a POSIX shell")
	}
	for _, name := range agentRefusedSampleNames {
		t.Setenv(name, "refused-host-value")
	}
	t.Setenv("ORDINARY_HOST_VAR", "ordinary")

	binDir := t.TempDir()
	envOut := filepath.Join(t.TempDir(), "env.txt")
	script := "#!/bin/sh\nenv > '" + envOut + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	target := filepath.Join(t.TempDir(), "file")
	writeFile(t, target, "content")
	if !gitTracked(target) {
		t.Fatal("gitTracked = false; the fake git did not run")
	}

	data, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatalf("read child env: %v", err)
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if k, _, ok := strings.Cut(line, "="); ok {
			names[k] = true
		}
	}
	for _, name := range agentRefusedSampleNames {
		if names[name] {
			t.Errorf("git child inherited %s", name)
		}
	}
	if !names["ORDINARY_HOST_VAR"] {
		t.Error("git child did not inherit ORDINARY_HOST_VAR")
	}
}
