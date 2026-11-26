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
	"strings"
	"testing"
)

func TestResolvedContainerID_ContextHelper(t *testing.T) {
	//nolint:staticcheck // explicitly verifying nil-context safety
	if resolvedContainerID(nil) {
		t.Fatal("resolvedContainerID(nil) = true, want false")
	}
	if resolvedContainerID(context.Background()) {
		t.Fatal("resolvedContainerID(Background) = true, want false")
	}
	if !resolvedContainerID(WithResolvedContainerID(context.Background())) {
		t.Fatal("resolvedContainerID(WithResolvedContainerID) = false, want true")
	}
}

// writeRecordingCLI writes a mock container CLI that appends each invocation
// ("list" or "exec <target-id>") to logPath. Its list subcommand prints
// listing; its exec subcommand drains stdin and exits 0.
func writeRecordingCLI(t *testing.T, listing string) (cliPath, logPath string) {
	t.Helper()
	dir := t.TempDir()
	listingPath := filepath.Join(dir, "listing")
	logPath = filepath.Join(dir, "calls.log")
	if err := os.WriteFile(listingPath, []byte(listing), 0o644); err != nil {
		t.Fatal(err)
	}
	cliPath = filepath.Join(dir, "mock-cli")
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
  ps|list)
    printf 'list\n' >> %q
    cat %q
    exit 0
    ;;
  exec)
    shift
    # Skip flags (-i, --user <val>) to capture the container ID argument.
    while [ $# -gt 0 ]; do
      case "$1" in
        -i) shift ;;
        --user) shift 2 ;;
        *) break ;;
      esac
    done
    printf 'exec %%s\n' "$1" >> %q
    cat >/dev/null
    exit 0
    ;;
esac
exit 1
`, logPath, listingPath, logPath)
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cliPath, logPath
}

func readCallLog(t *testing.T, logPath string) (lists int, execTargets []string) {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading call log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		if line == "list" {
			lists++
			continue
		}
		if target, ok := strings.CutPrefix(line, "exec "); ok {
			execTargets = append(execTargets, target)
			continue
		}
		t.Fatalf("unexpected call log line: %q", line)
	}
	return lists, execTargets
}

func TestContainerExec_ResolvedContainerIDSkipsList(t *testing.T) {
	for _, tc := range execRaceCases() {
		t.Run(tc.name+"/Exec/unmarked", func(t *testing.T) {
			cli, logPath := writeRecordingCLI(t, tc.listed)
			rt := tc.new(cli)
			if _, err := rt.Exec(context.Background(), "worker", []string{"true"}); err != nil {
				t.Fatalf("Exec(unmarked) error = %v", err)
			}
			lists, targets := readCallLog(t, logPath)
			if lists != 1 {
				t.Errorf("list calls = %d, want 1", lists)
			}
			if len(targets) != 1 || targets[0] != execRaceID {
				t.Errorf("exec targets = %v, want [%s]", targets, execRaceID)
			}
		})

		t.Run(tc.name+"/Exec/marked", func(t *testing.T) {
			cli, logPath := writeRecordingCLI(t, tc.listed)
			rt := tc.new(cli)
			ctx := WithResolvedContainerID(context.Background())
			if _, err := rt.Exec(ctx, execRaceID, []string{"true"}); err != nil {
				t.Fatalf("Exec(marked) error = %v", err)
			}
			lists, targets := readCallLog(t, logPath)
			if lists != 0 {
				t.Errorf("list calls = %d, want 0", lists)
			}
			if len(targets) != 1 || targets[0] != execRaceID {
				t.Errorf("exec targets = %v, want [%s]", targets, execRaceID)
			}
		})

		t.Run(tc.name+"/ExecWithStdin/unmarked", func(t *testing.T) {
			cli, logPath := writeRecordingCLI(t, tc.listed)
			rt := tc.new(cli)
			if _, err := rt.ExecWithStdin(context.Background(), "worker", []string{"cat"}, strings.NewReader("hello")); err != nil {
				t.Fatalf("ExecWithStdin(unmarked) error = %v", err)
			}
			lists, targets := readCallLog(t, logPath)
			if lists != 1 {
				t.Errorf("list calls = %d, want 1", lists)
			}
			if len(targets) != 1 || targets[0] != execRaceID {
				t.Errorf("exec targets = %v, want [%s]", targets, execRaceID)
			}
		})

		t.Run(tc.name+"/ExecWithStdin/marked", func(t *testing.T) {
			cli, logPath := writeRecordingCLI(t, tc.listed)
			rt := tc.new(cli)
			ctx := WithResolvedContainerID(context.Background())
			if _, err := rt.ExecWithStdin(ctx, execRaceID, []string{"cat"}, strings.NewReader("hello")); err != nil {
				t.Fatalf("ExecWithStdin(marked) error = %v", err)
			}
			lists, targets := readCallLog(t, logPath)
			if lists != 0 {
				t.Errorf("list calls = %d, want 0", lists)
			}
			if len(targets) != 1 || targets[0] != execRaceID {
				t.Errorf("exec targets = %v, want [%s]", targets, execRaceID)
			}
		})
	}
}

// Setting WithResolvedContainerID skips only the pre-exec List re-resolution;
// classifyExecErr must still list on a CLI not-found error so a container
// removed between the caller's lookup and Exec still maps to
// ErrContainerNotFound (ptone/scion#3655).
func TestContainerExec_ResolvedContainerIDKeepsNotFoundClassification(t *testing.T) {
	for _, tc := range execRaceCases() {
		t.Run(tc.name+"/removed", func(t *testing.T) {
			rt := tc.new(writeExecRaceCLI(t, tc.empty, 0, tc.notFound, tc.cliCode))
			ctx := WithResolvedContainerID(context.Background())
			_, err := rt.Exec(ctx, execRaceID, []string{"true"})
			if !errors.Is(err, ErrContainerNotFound) {
				t.Fatalf("Exec(marked) error = %v, want ErrContainerNotFound", err)
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				t.Errorf("Exec(marked) error %v must not wrap *exec.ExitError", err)
			}
		})

		t.Run(tc.name+"/command_exit", func(t *testing.T) {
			rt := tc.new(writeExecRaceCLI(t, tc.empty, 0, "sh: 1: foo: not found\n", 127))
			ctx := WithResolvedContainerID(context.Background())
			_, err := rt.Exec(ctx, execRaceID, []string{"foo"})
			if errors.Is(err, ErrContainerNotFound) {
				t.Fatalf("Exec(marked) error = %v, must not be ErrContainerNotFound", err)
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 127 {
				t.Fatalf("Exec(marked) error = %v, want *exec.ExitError with code 127", err)
			}
		})
	}
}
