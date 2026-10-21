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
	"strconv"
	"strings"
	"testing"
)

// writeKillRecorder writes a fake CLI at path that records the argv of its
// `kill` invocation to argvFile (other invocations, such as List's, are
// ignored) and exits killExit for it.
func writeKillRecorder(t *testing.T, path, argvFile string, killExit int) {
	t.Helper()
	script := `#!/bin/sh
if [ "$1" = "kill" ]; then
  printf '%s\n' "$@" > "` + argvFile + `"
  exit ` + strconv.Itoa(killExit) + `
fi
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake binary: %v", err)
	}
}

func initSignalerCases(bin string) map[string]InitSignaler {
	return map[string]InitSignaler{
		"docker": &DockerRuntime{Command: bin},
		"podman": &PodmanRuntime{Command: bin},
		"apple":  &AppleContainerRuntime{Command: bin},
	}
}

// TestSignalInitReload_Argv pins the exact runtime-side signal command for
// each CLI runtime, including the SIG-prefixed signal name (ptone/scion#5460).
func TestSignalInitReload_Argv(t *testing.T) {
	for name := range initSignalerCases("") {
		t.Run(name, func(t *testing.T) {
			tmpDir := t.TempDir()
			bin := filepath.Join(tmpDir, "mock-cli")
			argvFile := filepath.Join(tmpDir, "argv")
			writeKillRecorder(t, bin, argvFile, 0)

			sig := initSignalerCases(bin)[name]
			if err := sig.SignalInitReload(context.Background(), "agent-container"); err != nil {
				t.Fatalf("SignalInitReload: %v", err)
			}
			got, err := os.ReadFile(argvFile)
			if err != nil {
				t.Fatalf("kill was not invoked: %v", err)
			}
			if want := "kill\n--signal\nSIGUSR2\nagent-container\n"; string(got) != want {
				t.Errorf("argv = %q, want %q", string(got), want)
			}
		})
	}
}

func TestSignalInitReload_ErrorPropagates(t *testing.T) {
	for name := range initSignalerCases("") {
		t.Run(name, func(t *testing.T) {
			tmpDir := t.TempDir()
			bin := filepath.Join(tmpDir, "mock-cli")
			writeKillRecorder(t, bin, filepath.Join(tmpDir, "argv"), 1)

			if err := initSignalerCases(bin)[name].SignalInitReload(context.Background(), "agent-container"); err == nil {
				t.Fatal("expected an error when the kill command fails")
			}
		})
	}
}

func TestSignalInitReload_RefusesFlagLikeOrEmptyID(t *testing.T) {
	for _, id := range []string{"", "-a", "--all"} {
		tmpDir := t.TempDir()
		bin := filepath.Join(tmpDir, "mock-cli")
		argvFile := filepath.Join(tmpDir, "argv")
		writeKillRecorder(t, bin, argvFile, 0)

		for name, sig := range initSignalerCases(bin) {
			err := sig.SignalInitReload(context.Background(), id)
			if err == nil {
				t.Errorf("%s: SignalInitReload(%q) = nil, want an error", name, id)
			}
		}
		if _, err := os.Stat(argvFile); err == nil {
			t.Errorf("kill must not run for id %q", id)
		}
	}
}

func TestInitReloadSignalName(t *testing.T) {
	if initReloadSignal != "SIGUSR2" {
		t.Fatalf("initReloadSignal = %q; sciontool init reloads on SIGUSR2, passed by name", initReloadSignal)
	}
	args, err := initReloadKillArgs("c1")
	if err != nil || strings.Join(args, " ") != "kill --signal SIGUSR2 c1" {
		t.Fatalf("initReloadKillArgs = %v, %v", args, err)
	}
}
