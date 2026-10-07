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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Exec on a container removed between the broker's lookup and the exec
// (ptone/scion#3655): each container CLI's own not-found failure must
// surface as ErrContainerNotFound, while a command that ran and exited
// non-zero, whatever it printed, must stay the CLI's *exec.ExitError
// (ptone/scion#3470).

const (
	execRaceID      = "0123456789abcdef0123"
	execRaceOtherID = "fedcba9876543210fedc"
)

// writeExecRaceCLI writes a mock container CLI. Its list subcommand ("ps"
// for docker/podman, "list" for apple) prints listing and exits listCode;
// "exec" prints execOut on stderr and exits execCode.
func writeExecRaceCLI(t *testing.T, listing string, listCode int, execOut string, execCode int) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{"listing": listing, "exec-out": execOut} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cli := filepath.Join(dir, "mock-cli")
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
  ps|list) cat %q; exit %d ;;
  exec) cat %q >&2; exit %d ;;
esac
`, filepath.Join(dir, "listing"), listCode, filepath.Join(dir, "exec-out"), execCode)
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cli
}

type execRaceCase struct {
	name     string
	new      func(cli string) Runtime
	empty    string // list output with no containers
	listed   string // list output showing execRaceID
	notFound string // the CLI's not-found line for execRaceID
	cliCode  int    // the CLI's exit code for that failure
	// alsoNotFound are other not-found lines of this CLI for execRaceID
	// (each must classify like notFound).
	alsoNotFound []string
}

func execRaceCases() []execRaceCase {
	return []execRaceCase{
		{
			name:     "docker",
			new:      func(cli string) Runtime { return &DockerRuntime{Command: cli} },
			empty:    "",
			listed:   `{"ID":"` + execRaceID + `","Names":"proj--worker","Status":"Up 1 minute","Image":"img","Labels":"scion.name=worker"}` + "\n",
			notFound: "Error response from daemon: No such container: " + execRaceID + "\n",
			cliCode:  1,
		},
		{
			name:     "podman",
			new:      func(cli string) Runtime { return &PodmanRuntime{Command: cli} },
			empty:    "[]",
			listed:   `[{"Id":"` + execRaceID + `","Names":["proj--worker"],"Status":"running","Image":"img","Labels":{"scion.name":"worker"}}]`,
			notFound: `Error: no container with name or ID "` + execRaceID + `" found: no such container` + "\n",
			cliCode:  125,
			alsoNotFound: []string{
				`Error: no container with ID ` + execRaceID + ` found in database: no such container` + "\n",
			},
		},
		{
			name:     "apple",
			new:      func(cli string) Runtime { return &AppleContainerRuntime{Command: cli} },
			empty:    "[]",
			listed:   `[{"status":"running","configuration":{"id":"` + execRaceID + `","labels":{"scion.name":"worker"},"image":{"reference":"img"}}}]`,
			notFound: `Error: notFound: "get failed: container ` + execRaceID + ` not found"` + "\n",
			cliCode:  1,
			alsoNotFound: []string{
				`Error: notFound: "container with ID ` + execRaceID + ` not found"` + "\n",
			},
		},
	}
}

func TestContainerExec_RemovedAfterLookupIsContainerNotFound(t *testing.T) {
	for _, tc := range execRaceCases() {
		for i, out := range append([]string{tc.notFound}, tc.alsoNotFound...) {
			t.Run(fmt.Sprintf("%s/%d", tc.name, i), func(t *testing.T) {
				rt := tc.new(writeExecRaceCLI(t, tc.empty, 0, out, tc.cliCode))
				_, err := rt.Exec(context.Background(), execRaceID, []string{"true"})
				if !errors.Is(err, ErrContainerNotFound) {
					t.Fatalf("Exec error = %v, want ErrContainerNotFound", err)
				}
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					t.Errorf("Exec error %v must not wrap the CLI's *exec.ExitError (the broker would read it as the command's exit)", err)
				}
			})
		}
	}
}

func TestContainerExec_CommandExitIsNotContainerNotFound(t *testing.T) {
	for _, tc := range execRaceCases() {
		tests := []struct {
			name     string
			listing  string
			listCode int
			out      string
			code     int
		}{
			// The command itself failed with "not found" text.
			{"command not found", tc.empty, 0, "sh: 1: foo: not found\n", 127},
			{"command prints container wording", tc.empty, 0, "Error: container not found\n", tc.cliCode},
			// The CLI's exact wording, but from a different exit code: the
			// command's own exit, not the CLI's.
			{"cli wording with command exit code", tc.empty, 0, tc.notFound, 7},
			// The CLI's exact wording and exit code, but the container is
			// still there: the command printed it.
			{"cli wording but container still listed", tc.listed, 0, tc.notFound, tc.cliCode},
			// The CLI's exact wording and exit code, but the fresh list
			// failed: no evidence the container is gone, so it stays the
			// command's result.
			{"cli wording but list fails", tc.empty, 1, tc.notFound, tc.cliCode},
			// The CLI's wording for some other container (another full
			// hex ID for podman's database variant).
			{"cli wording for another id", tc.empty, 0, "Error response from daemon: No such container: other\n" +
				`Error: no container with name or ID "other" found: no such container` + "\n" +
				`Error: no container with ID ` + execRaceOtherID + ` found in database: no such container` + "\n" +
				`Error: notFound: "get failed: container other not found"` + "\n" +
				`Error: notFound: "container with ID other not found"` + "\n", tc.cliCode},
		}
		for _, tt := range tests {
			t.Run(tc.name+"/"+tt.name, func(t *testing.T) {
				rt := tc.new(writeExecRaceCLI(t, tt.listing, tt.listCode, tt.out, tt.code))
				_, err := rt.Exec(context.Background(), execRaceID, []string{"foo"})
				if errors.Is(err, ErrContainerNotFound) {
					t.Fatalf("Exec error = %v, must not be ErrContainerNotFound", err)
				}
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != tt.code {
					t.Fatalf("Exec error = %v, want the CLI's *exec.ExitError with code %d", err, tt.code)
				}
			})
		}
	}
}

// A cancelled ctx is no evidence the container is gone: classifyExecErr
// keeps the CLI's error and does not even list.
func TestClassifyExecErr_CancelledContextKeepsExitError(t *testing.T) {
	for _, tc := range execRaceCases() {
		t.Run(tc.name, func(t *testing.T) {
			// A real *exec.ExitError with the CLI's code.
			runErr := exec.Command("sh", "-c", fmt.Sprintf("exit %d", tc.cliCode)).Run()
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != tc.cliCode {
				t.Fatalf("setup: got %v, want exit %d", runErr, tc.cliCode)
			}
			n := map[string]containerExecNotFound{"docker": dockerExecNotFound, "podman": podmanExecNotFound, "apple": appleExecNotFound}[tc.name]
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			listed := false
			list := func(context.Context, map[string]string) ([]api.AgentInfo, error) {
				listed = true
				return nil, nil // "gone": only the ctx guard can keep the ExitError
			}
			err := n.classifyExecErr(ctx, runErr, tc.notFound, execRaceID, list)
			if errors.Is(err, ErrContainerNotFound) || err != runErr {
				t.Errorf("classifyExecErr = %v, want the original ExitError %v", err, runErr)
			}
			if listed {
				t.Errorf("classifyExecErr listed containers under a cancelled ctx")
			}
		})
	}
}
