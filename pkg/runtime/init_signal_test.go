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
	"errors"
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

func TestSignalInitReload_SharedPrefixRoutingAndAmbiguity(t *testing.T) {
	const (
		idLong   = "myproject--agent-10"
		idShort  = "myproject--agent-1"
		ambigPfx = "myproject--agent-"
	)
	listings := map[string]string{
		"docker": `{"ID":"` + idLong + `","Names":"` + idLong + `","Status":"Up 1 minute","Image":"img","Labels":"scion.name=agent-10"}` + "\n" +
			`{"ID":"` + idShort + `","Names":"` + idShort + `","Status":"Up 1 minute","Image":"img","Labels":"scion.name=agent-1"}` + "\n",
		"podman": `[{"Id":"` + idLong + `","Names":["` + idLong + `"],"Status":"running","Image":"img","Labels":{"scion.name":"agent-10"}},` +
			`{"Id":"` + idShort + `","Names":["` + idShort + `"],"Status":"running","Image":"img","Labels":{"scion.name":"agent-1"}}]`,
		"apple": `[{"status":"running","configuration":{"id":"` + idLong + `","labels":{"scion.name":"agent-10"},"image":{"reference":"img"}}},` +
			`{"status":"running","configuration":{"id":"` + idShort + `","labels":{"scion.name":"agent-1"},"image":{"reference":"img"}}}]`,
	}

	writeListAndKillCLI := func(t *testing.T, listing string) (bin, argvFile string) {
		t.Helper()
		dir := t.TempDir()
		listFile := filepath.Join(dir, "listing")
		if err := os.WriteFile(listFile, []byte(listing), 0o644); err != nil {
			t.Fatal(err)
		}
		argvFile = filepath.Join(dir, "argv")
		bin = filepath.Join(dir, "mock-cli")
		script := `#!/bin/sh
case "$1" in
  ps|list) cat "` + listFile + `"; exit 0 ;;
  kill) printf '%s\n' "$@" > "` + argvFile + `"; exit 0 ;;
esac
exit 1
`
		if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return bin, argvFile
	}

	for name, listing := range listings {
		t.Run(name+"/exact_shorter_wins", func(t *testing.T) {
			bin, argvFile := writeListAndKillCLI(t, listing)
			sig := initSignalerCases(bin)[name]
			if err := sig.SignalInitReload(context.Background(), idShort); err != nil {
				t.Fatalf("SignalInitReload: %v", err)
			}
			got, err := os.ReadFile(argvFile)
			if err != nil {
				t.Fatalf("kill was not invoked: %v", err)
			}
			if want := "kill\n--signal\nSIGUSR2\n" + idShort + "\n"; string(got) != want {
				t.Errorf("argv = %q, want %q", string(got), want)
			}
		})

		t.Run(name+"/ambiguous_prefix_refused", func(t *testing.T) {
			bin, argvFile := writeListAndKillCLI(t, listing)
			sig := initSignalerCases(bin)[name]
			err := sig.SignalInitReload(context.Background(), ambigPfx)
			if !errors.Is(err, ErrAmbiguousContainerID) {
				t.Fatalf("SignalInitReload error = %v, want ErrAmbiguousContainerID", err)
			}
			if _, statErr := os.Stat(argvFile); statErr == nil {
				t.Errorf("kill must not run on ambiguous prefix")
			}
		})
	}
}
