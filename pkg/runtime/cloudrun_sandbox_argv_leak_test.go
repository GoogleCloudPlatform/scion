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
	"strings"
	"testing"
)

// Tests for ptone/scion#1342: the error returned by CloudRunSandboxRuntime.Run
// when the sandbox binary fails must not carry the values of the --env pairs
// the runtime itself placed in argv.
//
// WHY THIS EXISTS AS A TEST RATHER THAN AN INVESTIGATION. #1342 records an open
// question -- "does the sandbox binary echo its own argv when it fails?" -- and
// says it "cannot be settled from Scion's source; it needs a look at the
// sandbox binary's error output." It has sat unresolved on the belief that
// answering it needs a live instance. It does not. A fake sandbox binary that
// echoes its argv turns "many CLIs might do this" into a measurement, and the
// measurement is cheap, hermetic and repeatable.
//
// THE TWO CASES BELOW ARE A DISCRIMINATING PAIR, NOT A DUPLICATE, AND THEY PIN
// TWO DIFFERENT THINGS. There are two independent routes by which a value in
// argv can reach the returned error, and a single test cannot tell you which
// one fired:
//
//	OUT  -- the sandbox binary echoes argv, and that output is spliced in by
//	        `(output: %s)` at cloudrun_sandbox_runtime.go:780. CONDITIONAL on
//	        the binary's behaviour. This is the route #1342 asks about and the
//	        one fixed here.
//	WRAP -- runSimpleCommand formats the full argv into the error it returns,
//	        and :780 wraps that error with %w. UNCONDITIONAL -- it does not
//	        depend on the binary's behaviour at all.
//
// WRAP was live on `main` and is closed by #127 P5, which reduced that error to
// `fmt.Errorf("%s failed: %w", command, err)`. This file is based on P5, so the
// silent case arrives GREEN and stays that way: it is a REGRESSION GUARD on
// P5's fix, asserted from the caller's side rather than from common.go's.
//
// Keep both cases permanently, and do not merge them. The pair is what makes
// the failure legible: OUT red with WRAP green says the sandbox binary echoed
// its argv, while both red would say argv had returned to the error text
// upstream. Collapsed into one case, those two very different regressions
// would be indistinguishable.
//
// Measured on `main` before the rebase, both cases were RED -- which is how we
// learned the answer to #1342's open question does not matter: the leak was
// unconditional there, so whether the binary echoes decided nothing.
const (
	// Per the standing constraint: never use a real credential in a test.
	argvLeakSentinel = "FAKE-KEY-SENTINEL-not-a-real-credential"

	// A distinctive line the fake binary always prints. Asserting this SURVIVES
	// is as load-bearing as asserting the sentinel does not.
	//
	// #22 and #46 were spent making sandbox failures diagnosable, and the
	// cheapest way to pass a leak test is to stop putting the subprocess output
	// in the error at all. That is a regression wearing a security fix's
	// costume, and this constant is what mechanically blocks it: a fix that
	// drops `out` turns this assertion red. The prohibition is in the test, not
	// only in the review guidance.
	argvLeakDiagnostic = "sandbox: fatal: mount target busy"
)

// writeFakeSandbox creates an executable stand-in for the sandbox binary that
// always fails. When echoArgv is true it also prints its full argv to stderr,
// imitating a CLI that reports the offending invocation on an argument error.
func writeFakeSandbox(t *testing.T, dir string, echoArgv bool) string {
	t.Helper()

	// Printed to stderr so it lands in CombinedOutput either way. `exit 1` is
	// what makes runSimpleCommand return an error and take the :779 branch.
	script := "#!/bin/sh\n" +
		"echo '" + argvLeakDiagnostic + "' >&2\n"
	if echoArgv {
		script += "printf '%s\\n' \"$@\" >&2\n"
	}
	script += "exit 1\n"

	path := filepath.Join(dir, "sandbox")
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// runWithSentinelEnv drives Run() against a failing fake sandbox with the
// sentinel supplied the way a real API key arrives: as a broker-provided
// RunConfig.Env entry, which envFor parses and envArgs turns into --env pairs.
func runWithSentinelEnv(t *testing.T, echoArgv bool) error {
	t.Helper()

	tmpDir := t.TempDir()
	mockBin := writeFakeSandbox(t, tmpDir, echoArgv)

	homeDir := filepath.Join(tmpDir, "agent-home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}

	rt := &CloudRunSandboxRuntime{
		bin:          mockBin,
		state:        newSandboxStateStore(filepath.Join(tmpDir, "state.json")),
		rootDir:      filepath.Join(tmpDir, "scion"),
		watchCancels: make(map[string]context.CancelFunc),
	}

	cfg := RunConfig{
		Name:      "leak-probe",
		HomeDir:   homeDir,
		Workspace: filepath.Join(tmpDir, "workspace"),
		Image:     "omni-image",
		Env:       []string{"ANTHROPIC_API_KEY=" + argvLeakSentinel},
		// REQUIRED, and its absence is not a detail. buildEntrypoint returns
		// "cloudrun-sandbox: no harness provided" and Run() gives up BEFORE it
		// ever executes the binary, so the first draft of this test went red
		// without reaching the leak site at all -- a vacuous red that would
		// have been reported as proof of the leak. The "error stays useful"
		// assertion is what caught it: the returned error came from Scion, not
		// from the subprocess.
		Harness: &mockHarness{command: []string{"/bin/true"}},
	}
	if err := os.MkdirAll(cfg.Workspace, 0755); err != nil {
		t.Fatal(err)
	}

	_, err := rt.Run(context.Background(), cfg)
	if err == nil {
		t.Fatal("Run() returned nil error, but the fake sandbox binary exits 1; " +
			"the test cannot say anything about error contents")
	}
	return err
}

func TestCloudRunSandboxRun_ErrorDoesNotLeakEnvValues(t *testing.T) {
	cases := []struct {
		name     string
		echoArgv bool
		route    string
	}{
		{
			name:     "sandbox binary echoes its argv on failure",
			echoArgv: true,
			route: "OUT -- output spliced in by (output: %s). This is the case #1342 " +
				"asks about and could not answer without a live instance.",
		},
		{
			name:     "sandbox binary is silent",
			echoArgv: false,
			route: "WRAP -- runSimpleCommand formats argv into its own error text. " +
				"The binary echoes NOTHING here, so a failure means the leak does " +
				"not depend on the sandbox binary's behaviour at all. This case is " +
				"GREEN on the #127 P5 base; if it is failing, P5's fix to " +
				"runSimpleCommand has regressed and the problem is NOT at :780.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runWithSentinelEnv(t, tc.echoArgv)
			got := err.Error()

			if strings.Contains(got, argvLeakSentinel) {
				t.Errorf("CREDENTIAL LEAK: Run() error contains the value of an --env pair.\n"+
					"route: %s\n"+
					"This error reaches an HTTP response body via handlers.go RuntimeError().\n"+
					"error: %s", tc.route, got)
			}

			// The error must remain diagnosable. See argvLeakDiagnostic.
			if !strings.Contains(got, argvLeakDiagnostic) {
				t.Errorf("error is no longer useful: it does not contain the sandbox binary's "+
					"diagnostic %q.\nRedaction must remove the credential VALUE, not the "+
					"subprocess output (see #22, #46).\nerror: %s", argvLeakDiagnostic, got)
			}
		})
	}
}

// The key name is not a secret and must survive redaction: "some value was
// removed here, and it was ANTHROPIC_API_KEY's" is a diagnostic, while a
// silently shortened string is a puzzle. Separate from the test above because
// it asserts a property of the FIX, not the absence of the leak.
func TestCloudRunSandboxRun_ErrorNamesTheRedactedKey(t *testing.T) {
	err := runWithSentinelEnv(t, true)
	got := err.Error()

	if strings.Contains(got, argvLeakSentinel) {
		t.Skip("leak still present; TestCloudRunSandboxRun_ErrorDoesNotLeakEnvValues owns that failure")
	}
	if !strings.Contains(got, "ANTHROPIC_API_KEY") {
		t.Errorf("redacted error does not name the key whose value was removed.\nerror: %s", got)
	}
}
