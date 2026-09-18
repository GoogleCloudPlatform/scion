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
if [[ -n "${FAKE_GO_BARRIER_DIR:-}" ]]; then
  touch "${FAKE_GO_BARRIER_DIR}/${TEST_INVOCATION_ID}"
  while [[ $(find "${FAKE_GO_BARRIER_DIR}" -type f | wc -l) -lt 2 ]]; do sleep 0.01; done
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
	legacyBefore, err := filepath.Glob("/tmp/ci-test-json.*")
	if err != nil {
		t.Fatal(err)
	}
	legacyKnown := make(map[string]bool, len(legacyBefore))
	for _, path := range legacyBefore {
		legacyKnown[path] = true
	}
	cmd := runnerCommand(t, tempParent, fakeRunnerBin(t, true), "FAKE_GO_READY="+ready)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner did not enter test phase:\n%s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("signal-interrupted runner succeeded")
	}
	requireNoRunnerArtifacts(t, tempParent)
	legacyAfter, err := filepath.Glob("/tmp/ci-test-json.*")
	if err != nil {
		t.Fatal(err)
	}
	var leaked []string
	for _, path := range legacyAfter {
		if !legacyKnown[path] {
			leaked = append(leaked, path)
			path := path
			t.Cleanup(func() { _ = os.Remove(path) })
		}
	}
	if len(leaked) != 0 {
		t.Fatalf("signal-interrupted runner leaked legacy logs: %v", leaked)
	}
}

func TestIntegrationRunnerConcurrentInvocationsAreIsolated(t *testing.T) {
	tempParent := t.TempDir()
	unrelated := filepath.Join(tempParent, "unrelated-runner-data")
	barrier := filepath.Join(tempParent, "barrier")
	for _, dir := range []string{unrelated, barrier} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	binDir := fakeRunnerBin(t, true)
	type result struct {
		id     string
		output []byte
		err    error
	}
	results := make(chan result, 2)
	for i := 1; i <= 2; i++ {
		id := fmt.Sprintf("runner-%d", i)
		cmd := runnerCommand(t, tempParent, binDir,
			"FAKE_GO_BARRIER_DIR="+barrier,
			"TEST_INVOCATION_ID="+id,
			"FAKE_CANARY_STATE="+filepath.Join(tempParent, id+"-canary-state"),
		)
		go func() {
			output, err := cmd.CombinedOutput()
			results <- result{id: id, output: output, err: err}
		}()
	}

	deadline := time.Now().Add(5 * time.Second)
	for len(runnerArtifacts(t, tempParent)) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("did not observe two isolated runner directories: %v", runnerArtifacts(t, tempParent))
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 0; i < 2; i++ {
		got := <-results
		if got.err != nil {
			t.Fatalf("%s failed: %v\n%s", got.id, got.err, got.output)
		}
	}
	requireNoRunnerArtifacts(t, tempParent)
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("concurrent runners removed unrelated directory: %v", err)
	}
}
