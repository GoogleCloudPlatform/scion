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
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStop_PassesStopTimeout pins the docker and podman stop argv: stop passes
// -t with the RunRef's StopTimeout in whole seconds (rounded up), and without
// one the default, which is longer than sciontool init's default grace period
// (10s) plus its final Hub report (5s), so the engine does not kill init
// before it exits cleanly (ptone/scion#3608).
func TestStop_PassesStopTimeout(t *testing.T) {
	if minWindow := 10*time.Second + 5*time.Second; defaultContainerStopTimeout <= minWindow {
		t.Errorf("default stop timeout = %s, want more than %s", defaultContainerStopTimeout, minWindow)
	}

	runtimes := map[string]func(bin string) Runtime{
		"docker": func(bin string) Runtime { return &DockerRuntime{Command: bin} },
		"podman": func(bin string) Runtime { return &PodmanRuntime{Command: bin} },
	}
	cases := []struct {
		name    string
		timeout time.Duration
		want    string
	}{
		{name: "default", want: "30"},
		{name: "set", timeout: 10 * time.Second, want: "10"},
		{name: "rounded up", timeout: 1500 * time.Millisecond, want: "2"},
		{name: "largest duration", timeout: time.Duration(math.MaxInt64), want: "9223372037"},
	}
	for name, newRT := range runtimes {
		for _, tc := range cases {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				tmpDir := t.TempDir()
				bin := filepath.Join(tmpDir, "mock-cli")
				argvFile := filepath.Join(tmpDir, "argv")
				script := `#!/bin/sh
if [ "$1" = "stop" ]; then
  printf '%s\n' "$@" > "` + argvFile + `"
  exit 0
fi
exit 1
`
				if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
					t.Fatalf("failed to write fake binary: %v", err)
				}

				ref := RunRef{ID: "agent-container", StopTimeout: tc.timeout}
				if err := newRT(bin).Stop(context.Background(), ref); err != nil {
					t.Fatalf("Stop: %v", err)
				}
				got, err := os.ReadFile(argvFile)
				if err != nil {
					t.Fatalf("stop was not invoked: %v", err)
				}
				want := "stop\n-t\n" + tc.want + "\nagent-container\n"
				if string(got) != want {
					t.Errorf("argv = %q, want %q", string(got), want)
				}
			})
		}
	}
}
