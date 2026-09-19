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

package integration_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const runnerTempPrefix = "scion-a2a-integration."

const runnerTestTimeout = 5 * time.Second

type runningRunner struct {
	cmd     *exec.Cmd
	output  bytes.Buffer
	done    chan struct{}
	waitErr error
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func fakeRunnerBin(t *testing.T, includePSQL bool) string {
	t.Helper()
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "go"), `#!/usr/bin/env bash
set -euo pipefail
if [[ -n "${FAKE_GO_READY:-}" ]]; then
  touch "${FAKE_GO_READY}"
  trap 'exit 143' TERM INT
  while :; do sleep 1; done
fi
if [[ -n "${FAKE_GO_PRE_BARRIER_STATUS:-}" ]]; then
  printf '%s\n' '{"Action":"output","Output":"injected pre-barrier failure\\n"}'
  exit "${FAKE_GO_PRE_BARRIER_STATUS}"
fi
if [[ -n "${FAKE_GO_BARRIER_DIR:-}" ]]; then
  touch "${FAKE_GO_BARRIER_DIR}/${TEST_INVOCATION_ID}.ready"
  while [[ ! -f "${FAKE_GO_BARRIER_RELEASE}" ]]; do sleep 0.01; done
fi
if [[ "${FAKE_GO_STATUS:-0}" != "0" ]]; then
  printf '%s\n' '{"Action":"output","Output":"injected early failure\\n"}'
  exit "${FAKE_GO_STATUS}"
fi
if [[ "${FAKE_GO_SKIP:-0}" == "1" ]]; then
  printf '%s\n' '{"Action":"skip","Test":"TestInjectedSkipFixture"}'
  exit 0
fi
printf '%s\n' '{"Action":"output","Output":"PASS\\n"}'
`)
	if includePSQL {
		writeExecutable(t, filepath.Join(binDir, "psql"), `#!/usr/bin/env bash
set -euo pipefail
if [[ "${FAKE_PSQL_STATUS:-0}" != "0" ]]; then exit "${FAKE_PSQL_STATUS}"; fi
case "$*" in
  *"SELECT value FROM test_canary.sentinel"*)
    if [[ -s "${FAKE_CANARY_STATE}" ]]; then cat "${FAKE_CANARY_STATE}"; else printf '%s\n' must-survive; fi
    ;;
  *"UPDATE test_canary.sentinel"*) printf '%s\n' mutated > "${FAKE_CANARY_STATE}" ;;
esac
`)
	}
	for _, name := range []string{"bash", "cat", "dirname", "find", "grep", "mktemp", "rm", "sed", "sleep", "touch", "wc"} {
		target, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("find required test utility %s: %v", name, err)
		}
		if err := os.Symlink(target, filepath.Join(binDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return binDir
}

func runnerCommand(t *testing.T, tempParent, binDir string, extraEnv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/bash", filepath.Join("..", "scripts", "run-integration-ci.sh"))
	cmd.Env = append(os.Environ(),
		"PATH="+binDir,
		"TMPDIR="+tempParent,
		"TEST_DATABASE_URL=postgres://runner-secret@invalid/test",
		"FAKE_CANARY_STATE="+filepath.Join(tempParent, "canary-state"),
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	return cmd
}

func startRunner(t *testing.T, cmd *exec.Cmd) *runningRunner {
	t.Helper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	runner := &runningRunner{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout = &runner.output
	cmd.Stderr = &runner.output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		runner.waitErr = cmd.Wait()
		close(runner.done)
	}()
	t.Cleanup(func() {
		if err := runner.terminateAndReap(syscall.SIGKILL, runnerTestTimeout); err != nil {
			t.Errorf("clean up runner process group: %v", err)
		}
	})
	return runner
}

func (r *runningRunner) wait(timeout time.Duration) (error, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-r.done:
		return r.waitErr, true
	case <-timer.C:
		return nil, false
	}
}

func (r *runningRunner) processGroupAlive() bool {
	err := syscall.Kill(-r.cmd.Process.Pid, 0)
	return err == nil || err == syscall.EPERM
}

func (r *runningRunner) terminateAndReap(signal syscall.Signal, timeout time.Duration) error {
	if r.processGroupAlive() {
		if err := syscall.Kill(-r.cmd.Process.Pid, signal); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("signal process group: %w", err)
		}
	}
	if _, finished := r.wait(timeout); !finished {
		if err := syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return fmt.Errorf("kill timed-out process group: %w", err)
		}
		if _, finished := r.wait(timeout); !finished {
			return fmt.Errorf("runner process was not reaped within %s", timeout)
		}
	}
	deadline := time.Now().Add(timeout)
	for r.processGroupAlive() {
		if time.Now().After(deadline) {
			return fmt.Errorf("runner process group survived for %s after reap", timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func waitForCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(runnerTestTimeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func runnerArtifacts(t *testing.T, tempParent string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(tempParent, runnerTempPrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func requireNoRunnerArtifacts(t *testing.T, tempParent string) {
	t.Helper()
	if matches := runnerArtifacts(t, tempParent); len(matches) != 0 {
		t.Fatalf("runner artifacts survived: %v", matches)
	}
}

func TestIntegrationRunnerCleansOwnedArtifacts(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		tempParent := t.TempDir()
		unrelated := filepath.Join(tempParent, "unrelated-runner-data")
		if err := os.Mkdir(unrelated, 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := runnerCommand(t, tempParent, fakeRunnerBin(t, true))
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("runner failed: %v\n%s", err, output)
		}
		if bytes.Contains(output, []byte("runner-secret")) {
			t.Fatalf("runner printed database credentials: %s", output)
		}
		requireNoRunnerArtifacts(t, tempParent)
		if _, err := os.Stat(unrelated); err != nil {
			t.Fatalf("runner removed unrelated directory: %v", err)
		}
	})

	for _, tc := range []struct {
		name     string
		psql     bool
		env      []string
		wantText string
	}{
		{name: "empty database URL", psql: true, env: []string{"TEST_DATABASE_URL="}, wantText: "unset or empty"},
		{name: "missing psql", psql: false, wantText: "psql is required"},
		{name: "early test failure", psql: true, env: []string{"FAKE_GO_STATUS=17"}, wantText: "failed with exit code 17"},
		{name: "forbidden skip", psql: true, env: []string{"FAKE_GO_SKIP=1"}, wantText: "forbidden test skip"},
		{name: "canary mutation", psql: true, env: []string{"TEST_TRIGGER_CANARY_MUTATION=1"}, wantText: "was mutated or deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempParent := t.TempDir()
			cmd := runnerCommand(t, tempParent, fakeRunnerBin(t, tc.psql), tc.env...)
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("runner succeeded; want failure\n%s", output)
			}
			if !strings.Contains(string(output), tc.wantText) {
				t.Fatalf("runner output missing %q:\n%s", tc.wantText, output)
			}
			requireNoRunnerArtifacts(t, tempParent)
		})
	}
}

func TestIntegrationRunnerCleansArtifactsOnSignal(t *testing.T) {
	tempParent := t.TempDir()
	ready := filepath.Join(tempParent, "go-ready")
	unrelated := filepath.Join(tempParent, "unrelated-runner-data")
	if err := os.Mkdir(unrelated, 0o755); err != nil {
		t.Fatal(err)
	}
	runner := startRunner(t, runnerCommand(t, tempParent, fakeRunnerBin(t, true), "FAKE_GO_READY="+ready))
	waitForCondition(t, "runner to enter the test phase", func() bool { return pathExists(ready) })

	unrelatedLegacyLog, err := os.CreateTemp("/tmp", "ci-test-json.")
	if err != nil {
		t.Fatal(err)
	}
	unrelatedLegacyLogPath := unrelatedLegacyLog.Name()
	if err := unrelatedLegacyLog.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(unrelatedLegacyLogPath) })

	if err := runner.terminateAndReap(syscall.SIGTERM, runnerTestTimeout); err != nil {
		t.Fatalf("terminate signal-interrupted runner: %v", err)
	}
	if runner.waitErr == nil {
		t.Fatal("signal-interrupted runner succeeded")
	}
	requireNoRunnerArtifacts(t, tempParent)
	if _, err := os.Stat(unrelatedLegacyLogPath); err != nil {
		t.Fatalf("signal-interrupted runner removed unrelated legacy-pattern file: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("signal-interrupted runner removed unrelated directory: %v", err)
	}
}

func TestIntegrationRunnerConcurrentInvocationsAreIsolated(t *testing.T) {
	t.Run("explicit release after both owned directories are observed", func(t *testing.T) {
		tempParent := t.TempDir()
		unrelated := filepath.Join(tempParent, "unrelated-runner-data")
		barrier := filepath.Join(tempParent, "barrier")
		for _, dir := range []string{unrelated, barrier} {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		release := filepath.Join(barrier, "release")
		binDir := fakeRunnerBin(t, true)
		runners := make([]*runningRunner, 0, 2)
		for i := 1; i <= 2; i++ {
			id := fmt.Sprintf("runner-%d", i)
			runners = append(runners, startRunner(t, runnerCommand(t, tempParent, binDir,
				"FAKE_GO_BARRIER_DIR="+barrier,
				"FAKE_GO_BARRIER_RELEASE="+release,
				"TEST_INVOCATION_ID="+id,
				"FAKE_CANARY_STATE="+filepath.Join(tempParent, id+"-canary-state"),
			)))
		}

		waitForCondition(t, "both runners to reach the barrier with isolated directories", func() bool {
			return len(runnerArtifacts(t, tempParent)) == 2 &&
				pathExists(filepath.Join(barrier, "runner-1.ready")) &&
				pathExists(filepath.Join(barrier, "runner-2.ready"))
		})
		for i, runner := range runners {
			if _, finished := runner.wait(10 * time.Millisecond); finished {
				t.Fatalf("runner-%d passed the barrier before explicit release", i+1)
			}
		}
		if err := os.WriteFile(release, nil, 0o644); err != nil {
			t.Fatal(err)
		}

		for i, runner := range runners {
			if err, finished := runner.wait(runnerTestTimeout); !finished {
				t.Fatalf("runner-%d did not exit within %s", i+1, runnerTestTimeout)
			} else if err != nil {
				t.Fatalf("runner-%d failed: %v\n%s", i+1, err, runner.output.String())
			}
		}
		requireNoRunnerArtifacts(t, tempParent)
		if _, err := os.Stat(unrelated); err != nil {
			t.Fatalf("concurrent runners removed unrelated directory: %v", err)
		}
	})

	t.Run("forced timeout terminates and reaps the process group", func(t *testing.T) {
		tempParent := t.TempDir()
		unrelated := filepath.Join(tempParent, "unrelated-runner-data")
		if err := os.Mkdir(unrelated, 0o755); err != nil {
			t.Fatal(err)
		}
		ready := filepath.Join(tempParent, "go-ready")
		runner := startRunner(t, runnerCommand(t, tempParent, fakeRunnerBin(t, true), "FAKE_GO_READY="+ready))
		waitForCondition(t, "runner to enter the held test phase", func() bool {
			return pathExists(ready) && len(runnerArtifacts(t, tempParent)) == 1
		})
		if _, finished := runner.wait(50 * time.Millisecond); finished {
			t.Fatalf("held runner exited before the forced timeout:\n%s", runner.output.String())
		}
		if err := runner.terminateAndReap(syscall.SIGTERM, runnerTestTimeout); err != nil {
			t.Fatalf("terminate timed-out runner: %v", err)
		}
		if runner.waitErr == nil {
			t.Fatal("timed-out runner succeeded after termination")
		}
		if runner.processGroupAlive() {
			t.Fatal("timed-out runner process group survived termination")
		}
		requireNoRunnerArtifacts(t, tempParent)
		if _, err := os.Stat(unrelated); err != nil {
			t.Fatalf("timeout cleanup removed unrelated directory: %v", err)
		}
	})

	t.Run("pre-barrier failure cleans every runner", func(t *testing.T) {
		tempParent := t.TempDir()
		unrelated := filepath.Join(tempParent, "unrelated-runner-data")
		barrier := filepath.Join(tempParent, "barrier")
		for _, dir := range []string{unrelated, barrier} {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		release := filepath.Join(barrier, "release")
		binDir := fakeRunnerBin(t, true)
		held := startRunner(t, runnerCommand(t, tempParent, binDir,
			"FAKE_GO_BARRIER_DIR="+barrier,
			"FAKE_GO_BARRIER_RELEASE="+release,
			"TEST_INVOCATION_ID=held",
			"FAKE_CANARY_STATE="+filepath.Join(tempParent, "held-canary-state"),
		))
		failed := startRunner(t, runnerCommand(t, tempParent, binDir,
			"FAKE_GO_BARRIER_DIR="+barrier,
			"FAKE_GO_BARRIER_RELEASE="+release,
			"FAKE_GO_PRE_BARRIER_STATUS=23",
			"TEST_INVOCATION_ID=failed",
			"FAKE_CANARY_STATE="+filepath.Join(tempParent, "failed-canary-state"),
		))
		waitForCondition(t, "first runner to reach the barrier", func() bool {
			return pathExists(filepath.Join(barrier, "held.ready"))
		})
		if err, finished := failed.wait(runnerTestTimeout); !finished {
			t.Fatal("pre-barrier failure did not exit within the bounded wait")
		} else if err == nil {
			t.Fatal("pre-barrier failure unexpectedly succeeded")
		}
		if !strings.Contains(failed.output.String(), "failed with exit code 23") {
			t.Fatalf("pre-barrier failure output missing exit status:\n%s", failed.output.String())
		}
		if pathExists(filepath.Join(barrier, "failed.ready")) {
			t.Fatal("failing runner reached the barrier")
		}
		if err := held.terminateAndReap(syscall.SIGTERM, runnerTestTimeout); err != nil {
			t.Fatalf("terminate runner held by failed peer: %v", err)
		}
		for _, runner := range []*runningRunner{held, failed} {
			if runner.processGroupAlive() {
				t.Fatal("runner process group survived pre-barrier failure cleanup")
			}
		}
		requireNoRunnerArtifacts(t, tempParent)
		if _, err := os.Stat(unrelated); err != nil {
			t.Fatalf("pre-barrier cleanup removed unrelated directory: %v", err)
		}
	})
}
