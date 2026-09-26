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

package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/spf13/cobra"
)

// TestBootsServerInProcess guards the bootsServerInProcess skip predicate:
// disabling it (`return false` unconditionally) breaks the "server start" /
// "runtime-broker start" cases below, and widening it to the whole subtree
// (dropping the cmd.Name() == "start" check) breaks the "server status" /
// "server restart" / "runtime-broker status" cases — both mutations are
// caught by this single table.
func TestBootsServerInProcess(t *testing.T) {
	tests := []struct {
		name string
		args []string // nil means "pass a nil *cobra.Command"
		want bool
	}{
		{name: "server start", args: []string{"server", "start"}, want: true},
		{name: "runtime-broker start", args: []string{"runtime-broker", "start"}, want: true},
		{name: "server status", args: []string{"server", "status"}, want: false},
		{name: "server restart", args: []string{"server", "restart"}, want: false},
		{name: "runtime-broker status", args: []string{"runtime-broker", "status"}, want: false},
		{name: "top-level start <agent>", args: []string{"start", "some-agent"}, want: false},
		{name: "project list", args: []string{"project", "list"}, want: false},
		{name: "nil command", args: nil, want: false},
		// A persistent flag with a separate value, before the subcommand,
		// must not be mistaken for it (cobra's Find/stripFlags strips a
		// registered flag's value token before matching subcommand names).
		{name: "global flag+value before server start (--format json)", args: []string{"--format", "json", "server", "start"}, want: true},
		{name: "global short flag+value before server start (-g x)", args: []string{"-g", "x", "server", "start"}, want: true},
		{name: "global flag=value before server start", args: []string{"--project=x", "server", "start"}, want: true},
		{name: "global bool flag before server start (--global)", args: []string{"--global", "server", "start"}, want: true},
		{name: "global flag+value before runtime-broker start", args: []string{"--format", "json", "runtime-broker", "start"}, want: true},
		{name: "global flag+value before server status (not start)", args: []string{"--format", "json", "server", "status"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resolved *cobra.Command
			if tt.args != nil {
				found, _, err := rootCmd.Find(tt.args)
				if err != nil {
					t.Fatalf("rootCmd.Find(%v) = %v", tt.args, err)
				}
				resolved = found
			}
			if got := bootsServerInProcess(resolved); got != tt.want {
				t.Errorf("bootsServerInProcess(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestStderrReporter_EnvIgnoredMessage(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })

	stderrReporter{}.EnvIgnored("SCION_HUB_GROVE_ID", "SCION_HUB_PROJECT_ID")

	_ = w.Close()
	out, _ := io.ReadAll(r)
	got := strings.TrimSpace(string(out))
	want := "scion: SCION_HUB_GROVE_ID is no longer read; set SCION_HUB_PROJECT_ID instead"
	if got != want {
		t.Fatalf("EnvIgnored wrote %q, want %q", got, want)
	}
}

// TestWarnRemovedLegacyEnv_StderrOnlyUnderJSON is a cmd-level check that the
// legacy-env warning (wired into rootCmd.PersistentPreRunE in root.go) goes
// to stderr only: with --format json, stdout must stay valid, parseable
// JSON with no warning text mixed in.
func TestWarnRemovedLegacyEnv_StderrOnlyUnderJSON(t *testing.T) {
	t.Setenv("SCION_HUB_GROVE_ID", "legacy-uuid")
	// Neutralize checks that are unrelated to this test but would otherwise
	// depend on the environment scion happens to run in (agent-container
	// detection, a configured Hub endpoint).
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_HUB_URL", "")
	t.Setenv("HOME", t.TempDir())

	// The warning is sync.Once-guarded per process; reset it so this test
	// observes it regardless of test execution order.
	warnRemovedLegacyEnvOnce = sync.Once{}
	t.Cleanup(func() { warnRemovedLegacyEnvOnce = sync.Once{} })

	savedFormat, savedProjectPath, savedGlobal, savedNoHub, savedHubEndpoint :=
		outputFormat, projectPath, globalMode, noHub, hubEndpoint
	t.Cleanup(func() {
		outputFormat, projectPath, globalMode, noHub, hubEndpoint =
			savedFormat, savedProjectPath, savedGlobal, savedNoHub, savedHubEndpoint
	})

	origStdout, origStderr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errW
	// Cleanup, not just the explicit restore below, so a panic or t.Fatal
	// inside rootCmd.Execute() can't leave every later test in this binary
	// writing to a closed pipe.
	t.Cleanup(func() { os.Stdout, os.Stderr = origStdout, origStderr })

	rootCmd.SetArgs([]string{"project", "list", "--format", "json"})
	t.Cleanup(func() { rootCmd.SetArgs(nil) })
	runErr := rootCmd.Execute()

	os.Stdout, os.Stderr = origStdout, origStderr
	_ = outW.Close()
	_ = errW.Close()
	stdoutBytes, _ := io.ReadAll(outR)
	stderrBytes, _ := io.ReadAll(errR)

	if runErr != nil {
		t.Fatalf("rootCmd.Execute() = %v", runErr)
	}

	stderrText := string(stderrBytes)
	wantWarning := "scion: SCION_HUB_GROVE_ID is no longer read; set SCION_HUB_PROJECT_ID instead"
	if !strings.Contains(stderrText, wantWarning) {
		t.Fatalf("stderr = %q, want it to contain %q", stderrText, wantWarning)
	}

	stdoutText := string(stdoutBytes)
	if strings.Contains(stdoutText, "scion:") || strings.Contains(stdoutText, "GROVE") {
		t.Fatalf("stdout leaked the legacy-env warning: %q", stdoutText)
	}
	var projects []json.RawMessage
	if err := json.Unmarshal(stdoutBytes, &projects); err != nil {
		t.Fatalf("stdout is not valid JSON: %v (stdout=%q)", err, stdoutText)
	}
}

// captureStdIO redirects os.Stdout and os.Stderr for the duration of fn and
// returns everything written to each.
func captureStdIO(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outW, errW
	// Cleanup, not just the explicit restore below, so a panic or t.Fatal
	// inside fn can't leave every later test in this binary writing to a
	// closed pipe. It also closes the write ends (ignoring errors, since the
	// explicit Close calls below may already have run) so a t.Fatal inside
	// fn can't leave the drain goroutines parked on a pipe whose write end
	// is never closed.
	t.Cleanup(func() {
		os.Stdout, os.Stderr = origOut, origErr
		_ = outW.Close()
		_ = errW.Close()
	})

	// Drain both pipes concurrently so a caller that writes more than the
	// pipe buffer holds can't deadlock: io.Copy on each pipe blocks until
	// its write end is closed, so both must be read before that close can
	// be waited on.
	var wg sync.WaitGroup
	var outBuf, errBuf strings.Builder
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&outBuf, outR)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&errBuf, errR)
	}()

	fn()
	os.Stdout, os.Stderr = origOut, origErr
	_ = outW.Close()
	_ = errW.Close()
	wg.Wait()
	return outBuf.String(), errBuf.String()
}

// TestProjectMigration_StderrOnly is the CLI-side check that migrating a
// project's .scion/grove-id (config.ReadProjectID, via
// config.MigrateLegacyProject) writes its "scion: migrated ..." line to
// stderr only, using the same stderrReporter the CLI boot hook installs
// (config.SetProjectMigrationReporter in warnRemovedLegacyEnv), and that a
// second read of the same directory in the same process is silent.
func TestProjectMigration_StderrOnly(t *testing.T) {
	config.SetProjectMigrationReporter(stderrReporter{})
	t.Cleanup(func() { config.SetProjectMigrationReporter(config.NewSlogReporter()) })

	scionDir := filepath.Join(t.TempDir(), "myproj", ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scionDir, "grove-id"), []byte("legacy-uuid\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var id string
	var readErr error
	stdout, stderr := captureStdIO(t, func() {
		id, readErr = config.ReadProjectID(scionDir)
	})

	if readErr != nil {
		t.Fatalf("ReadProjectID: %v", readErr)
	}
	if id != "legacy-uuid" {
		t.Errorf("ReadProjectID = %q, want %q", id, "legacy-uuid")
	}
	if stdout != "" {
		t.Errorf("stdout leaked migration output: %q", stdout)
	}
	wantLine := "scion: migrated " + filepath.Join(scionDir, "grove-id") +
		" -> " + filepath.Join(scionDir, "project-id") + "\n"
	if stderr != wantLine {
		t.Errorf("stderr = %q, want %q", stderr, wantLine)
	}

	// Second read of the same directory, same process: silent (the event
	// was already reported once for this directory).
	stdout2, stderr2 := captureStdIO(t, func() {
		_, _ = config.ReadProjectID(scionDir)
	})
	if stdout2 != "" || stderr2 != "" {
		t.Errorf("second read was not silent: stdout=%q stderr=%q", stdout2, stderr2)
	}
}

// TestProjectMigration_ConflictNamesPathsAndRemedy is the CLI-side check
// that a grove-id/project-id conflict warning names both paths and a
// remedy, not just "(current behaviour)": stderrReporter.Conflict prints
// config.MigrateLegacyProject's detail verbatim, so this pins the format at
// the integration point a user actually sees.
func TestProjectMigration_ConflictNamesPathsAndRemedy(t *testing.T) {
	config.SetProjectMigrationReporter(stderrReporter{})
	t.Cleanup(func() { config.SetProjectMigrationReporter(config.NewSlogReporter()) })

	scionDir := filepath.Join(t.TempDir(), "myproj", ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(scionDir, "grove-id")
	newPath := filepath.Join(scionDir, "project-id")
	if err := os.WriteFile(oldPath, []byte("legacy-uuid"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("canonical-uuid"), 0644); err != nil {
		t.Fatal(err)
	}

	var id string
	var readErr error
	stdout, stderr := captureStdIO(t, func() {
		id, readErr = config.ReadProjectID(scionDir)
	})

	if readErr != nil {
		t.Fatalf("ReadProjectID: %v", readErr)
	}
	if id != "canonical-uuid" {
		t.Errorf("ReadProjectID = %q, want %q (canonical wins)", id, "canonical-uuid")
	}
	if stdout != "" {
		t.Errorf("stdout leaked migration output: %q", stdout)
	}
	wantLine := "scion: warning: both " + oldPath + " and " + newPath +
		" exist with different values; using project-id. " +
		"Remove " + oldPath + " (after checking its value) to silence this warning.\n"
	if stderr != wantLine {
		t.Errorf("stderr = %q, want %q", stderr, wantLine)
	}
}
