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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func TestDockerRuntime_Run_NoInitFlag(t *testing.T) {
	// Create a temporary script to act as a mock docker
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")

	script := `#!/bin/sh
echo "$@"
`
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock docker: %v", err)
	}

	runtime := &DockerRuntime{
		Command: mockDocker,
	}

	config := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Task:         "hello",
	}

	out, err := runtime.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("runtime.Run failed: %v", err)
	}

	// sciontool handles PID 1 responsibilities, so --init should NOT be present
	if strings.Contains(out, "--init") {
		t.Errorf("expected '--init' to be absent in output, got %q", out)
	}

	if !strings.Contains(out, "run -t") {
		t.Errorf("expected 'run -t' in output, got %q", out)
	}
}

func TestDockerRuntime_Exec_UserFlag(t *testing.T) {
	// Create a temporary script to act as a mock docker
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")

	script := `#!/bin/sh
echo "$@"
`
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock docker: %v", err)
	}

	runtime := &DockerRuntime{
		Command: mockDocker,
	}

	out, err := runtime.Exec(context.Background(), "test-container", []string{"whoami"})
	if err != nil {
		t.Fatalf("runtime.Exec failed: %v", err)
	}

	if !strings.Contains(out, "--user scion") {
		t.Errorf("expected '--user scion' in exec output, got %q", out)
	}
}

// TestDockerRuntime_List_FormatAvoidsSize guards ptone/scion#1867: the ps
// format must not reference .Size (directly or via "{{json .}}"), because that
// makes the daemon compute container sizes, which fails intermittently on
// hosts with high overlay churn.
func TestDockerRuntime_List_FormatAvoidsSize(t *testing.T) {
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")
	argsFile := filepath.Join(tmpDir, "args")

	// Record the args, then emit one container line in the shape the real
	// format produces.
	script := `#!/bin/sh
printf '%s\n' "$@" > "` + argsFile + `"
echo '{"ID":"abc123","Names":"proj--agent1","Status":"Up 5 minutes","Image":"scion-claude:latest","Labels":"scion.name=agent1,scion.template=developer"}'
`
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock docker: %v", err)
	}

	rt := &DockerRuntime{Command: mockDocker}
	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("failed to read recorded args: %v", err)
	}
	args := string(raw)
	if strings.Contains(args, "{{json .}}") {
		t.Errorf("ps format must not use {{json .}} (triggers size calculation); args: %q", args)
	}
	if strings.Contains(args, ".Size") {
		t.Errorf("ps format must not reference .Size; args: %q", args)
	}

	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	a := agents[0]
	if a.ContainerID != "abc123" || a.Name != "agent1" || a.Image != "scion-claude:latest" ||
		a.ContainerStatus != "Up 5 minutes" || a.Template != "developer" {
		t.Errorf("unexpected parsed agent: %+v", a)
	}
}

// writeFailNTimesScript writes a mock docker binary that fails the first
// failCount invocations (recorded in a counter file) with exit status 1,
// then succeeds and emits one container line.
func writeFailNTimesScript(t *testing.T, dir string, failCount int) (mockDocker, counterFile string) {
	t.Helper()
	mockDocker = filepath.Join(dir, "mock-docker")
	counterFile = filepath.Join(dir, "counter")
	if err := os.WriteFile(counterFile, []byte("0"), 0644); err != nil {
		t.Fatalf("failed to seed counter file: %v", err)
	}

	script := `#!/bin/sh
n=$(cat "` + counterFile + `")
n=$((n + 1))
echo "$n" > "` + counterFile + `"
if [ "$n" -le ` + strconv.Itoa(failCount) + ` ]; then
  echo "ps failed: transient snapshotter error" >&2
  exit 1
fi
echo '{"ID":"abc123","Names":"proj--agent1","Status":"Up 5 minutes","Image":"scion-claude:latest","Labels":"scion.name=agent1"}'
`
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock docker: %v", err)
	}
	return mockDocker, counterFile
}

func readCounter(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read counter file: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("failed to parse counter file %q: %v", string(raw), err)
	}
	return n
}

func TestDockerRuntime_List_RetriesTransientFailure(t *testing.T) {
	tmpDir := t.TempDir()
	mockDocker, counterFile := writeFailNTimesScript(t, tmpDir, 1) // fails once, then succeeds

	rt := &DockerRuntime{Command: mockDocker}
	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List should have recovered after one retry, got error: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "agent1" {
		t.Fatalf("unexpected agents after retry: %+v", agents)
	}

	if got := readCounter(t, counterFile); got != 2 {
		t.Fatalf("expected exactly 2 invocations (1 failure + 1 success), got %d", got)
	}
}

func TestDockerRuntime_List_GivesUpAfterMaxAttempts(t *testing.T) {
	tmpDir := t.TempDir()
	// Always fails: more than dockerListMaxAttempts failures available.
	mockDocker, counterFile := writeFailNTimesScript(t, tmpDir, dockerListMaxAttempts+5)

	rt := &DockerRuntime{Command: mockDocker}
	_, err := rt.List(context.Background(), nil)
	if err == nil {
		t.Fatal("expected List to return an error once retries are exhausted")
	}
	if !strings.Contains(err.Error(), "docker ps failed") {
		t.Errorf("expected error to mention docker ps failure, got: %v", err)
	}

	if got := readCounter(t, counterFile); got != dockerListMaxAttempts {
		t.Fatalf("expected exactly %d invocations (bounded retry), got %d", dockerListMaxAttempts, got)
	}
}

func TestDockerRuntime_List_StopsRetryingWhenContextDone(t *testing.T) {
	tmpDir := t.TempDir()
	mockDocker, counterFile := writeFailNTimesScript(t, tmpDir, dockerListMaxAttempts+5)

	rt := &DockerRuntime{Command: mockDocker}
	// Cancel before the first retry's backoff can elapse so we can assert
	// the loop doesn't keep sleeping/retrying past a dead context.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := rt.List(ctx, nil)
	if err == nil {
		t.Fatal("expected an error when the context is cancelled mid-retry")
	}

	// Give a generous margin, but well under what dockerListMaxAttempts
	// worth of full backoff would take, to confirm we didn't keep retrying.
	if got := readCounter(t, counterFile); got >= dockerListMaxAttempts {
		t.Fatalf("expected fewer than %d invocations once context is done, got %d", dockerListMaxAttempts, got)
	}
}

// TestDockerRuntime_List_CallerCancelDoesNotAbortOthers guards against
// List's singleflight group running its shared `docker ps` exec under
// whichever caller's context happened to start it: if that caller cancels,
// every other caller collapsed into the same call must still get the
// result rather than an error caused by someone else's cancellation.
func TestDockerRuntime_List_CallerCancelDoesNotAbortOthers(t *testing.T) {
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")

	// Sleep long enough that both callers are guaranteed to have joined the
	// same singleflight call before either the cancellation or the exec
	// completes.
	script := `#!/bin/sh
sleep 0.3
echo '{"ID":"abc123","Names":"proj--agent1","Status":"Up 5 minutes","Image":"scion-claude:latest","Labels":"scion.name=agent1"}'
`
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock docker: %v", err)
	}

	rt := &DockerRuntime{Command: mockDocker}

	cancelledCtx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(2)

	var cancelledErr error
	go func() {
		defer wg.Done()
		_, cancelledErr = rt.List(cancelledCtx, nil)
	}()

	var survivorErr error
	var survivorAgents []api.AgentInfo
	go func() {
		defer wg.Done()
		// Give the first goroutine a head start so it's the one that starts
		// the singleflight call, then join before it resolves.
		time.Sleep(20 * time.Millisecond)
		survivorAgents, survivorErr = rt.List(context.Background(), nil)
	}()

	// Cancel well before the mock's 0.3s sleep elapses, but after both
	// callers have joined the shared call.
	time.Sleep(100 * time.Millisecond)
	cancel()

	wg.Wait()

	if cancelledErr == nil {
		t.Fatal("expected the cancelled caller to receive an error")
	}
	if !errors.Is(cancelledErr, context.Canceled) {
		t.Fatalf("expected cancelled caller's error to wrap context.Canceled, got: %v", cancelledErr)
	}

	if survivorErr != nil {
		t.Fatalf("expected the other caller to succeed despite the first caller's cancellation, got: %v", survivorErr)
	}
	if len(survivorAgents) != 1 || survivorAgents[0].Name != "agent1" {
		t.Fatalf("unexpected agents from surviving caller: %+v", survivorAgents)
	}
}

func TestDockerRuntime_List_CollapsesConcurrentCalls(t *testing.T) {
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")
	counterFile := filepath.Join(tmpDir, "counter")
	if err := os.WriteFile(counterFile, []byte("0"), 0644); err != nil {
		t.Fatalf("failed to seed counter file: %v", err)
	}

	// Sleep briefly so concurrent List() calls are guaranteed to overlap
	// and race into the singleflight group together.
	script := `#!/bin/sh
n=$(cat "` + counterFile + `")
n=$((n + 1))
echo "$n" > "` + counterFile + `"
sleep 0.1
echo '{"ID":"abc123","Names":"proj--agent1","Status":"Up 5 minutes","Image":"scion-claude:latest","Labels":"scion.name=agent1"}'
`
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock docker: %v", err)
	}

	rt := &DockerRuntime{Command: mockDocker}

	const callers = 10
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := rt.List(context.Background(), nil); err != nil {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()

	if n := failures.Load(); n != 0 {
		t.Fatalf("expected all concurrent List calls to succeed, got %d failures", n)
	}
	if got := readCounter(t, counterFile); got != 1 {
		t.Fatalf("expected singleflight to collapse %d concurrent calls into 1 exec, got %d", callers, got)
	}
}
