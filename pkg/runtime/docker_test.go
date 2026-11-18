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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
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

// TestDockerRuntime_Run_ContextCancelled_RollsBackPartialContainer covers
// ptone/scion#1886: if the caller's context is cancelled while `docker run
// -d` is still talking to the daemon, killing the CLI subprocess alone does
// not undo a container the daemon may have already created/started. Run
// must roll that back itself with a best-effort `docker rm -f` so a
// cancelled create (e.g. because the Hub gave up waiting) doesn't leak a
// running container.
func TestDockerRuntime_Run_ContextCancelled_RollsBackPartialContainer(t *testing.T) {
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")
	rmLog := filepath.Join(tmpDir, "rm.log")

	// The mock "run" hangs until killed by context cancellation, simulating
	// a slow create (e.g. a cold-start container/sandbox build) that
	// outlives the caller's patience. The mock "rm" records its arguments
	// so the test can confirm the rollback targeted the right container.
	// "exec sleep" replaces the shell process in place rather than forking a
	// child, so killing this process (what exec.CommandContext does on ctx
	// cancellation) takes effect immediately instead of leaving an orphaned
	// grandchild holding the output pipe open.
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "run" ]; then
  exec sleep 30
elif [ "$1" = "rm" ]; then
  echo "$@" >> %q
fi
`, rmLog)
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock docker: %v", err)
	}

	runtime := &DockerRuntime{
		Command: mockDocker,
	}

	config := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent-cancel",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Task:         "hello",
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	_, err := runtime.Run(ctx, config)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected Run to return context.Canceled, got %v", err)
	}

	// rollbackCancelledCreate runs synchronously (on a fresh, uncancelled
	// context) before Run returns, so rm.log must already exist.
	data, readErr := os.ReadFile(rmLog)
	if readErr != nil {
		t.Fatalf("expected a rollback 'docker rm' after cancellation, but rm.log was not written: %v", readErr)
	}
	if !strings.Contains(string(data), "test-agent-cancel") {
		t.Errorf("expected rollback rm to target container 'test-agent-cancel', got %q", string(data))
	}
	if !strings.Contains(string(data), "-f") {
		t.Errorf("expected rollback rm to force-remove, got %q", string(data))
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

// TestDockerRuntime_List_LabelFiltering_ProjectPathThroughMigratedSymlink
// proves that, for the docker runtime, an agent whose scion.project_path
// label still holds a project's pre-rename path (recorded
// on the container before config.MigrateLegacyGlobalLayout moved that
// project) is still found when listing filters by the project's current,
// canonical path. The legacy layout is built with the real migrator, which
// symlinks each entry it moves individually — the legacy root itself stays a
// real directory — not with a hand-made root-level symlink.
func TestDockerRuntime_List_LabelFiltering_ProjectPathThroughMigratedSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	legacyProjectPath := filepath.Join(home, ".scion", "groves", "proj")
	if err := os.MkdirAll(legacyProjectPath, 0755); err != nil {
		t.Fatal(err)
	}
	config.MigrateLegacyGlobalLayout(filepath.Join(home, ".scion"), noopGlobalLayoutReporter{})

	canonicalProjectPath := filepath.Join(home, ".scion", "projects", "proj")
	if _, err := os.Stat(canonicalProjectPath); err != nil {
		t.Fatalf("migration did not create the canonical project dir: %v", err)
	}
	siblingPath := filepath.Join(home, ".scion", "projects", "proj-2")
	if err := os.MkdirAll(siblingPath, 0755); err != nil {
		t.Fatal(err)
	}

	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")
	script := `#!/bin/sh
echo '{"ID":"aaa","Names":"agent-a","Status":"Up","Image":"img","Labels":"scion.project_path=` + legacyProjectPath + `"}'
`
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock docker: %v", err)
	}
	rt := &DockerRuntime{Command: mockDocker}

	agents, err := rt.List(context.Background(), map[string]string{"scion.project_path": canonicalProjectPath})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "agent-a" {
		t.Fatalf("expected agent-a to match through the migrated symlink, got %+v", agents)
	}

	agents, err = rt.List(context.Background(), map[string]string{"scion.project_path": siblingPath})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("expected no match against a sibling project path, got %+v", agents)
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

	// Write the counter via a temp file and rename so a concurrent
	// readCounter never sees a truncated, empty file (ptone/scion#3694).
	script := `#!/bin/sh
n=$(cat "` + counterFile + `")
n=$((n + 1))
echo "$n" > "` + counterFile + `.tmp.$$"
mv -f "` + counterFile + `.tmp.$$" "` + counterFile + `"
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
	// List returns as soon as the caller's ctx is done, but the shared
	// singleflight exec keeps retrying on its own detached context and keeps
	// writing the counter file in tmpDir. Without this, t.TempDir's RemoveAll
	// can race that straggler and fail with "directory not empty"
	// (ptone/scion#3694). Cleanups run LIFO, so this drain runs before the
	// TempDir removal registered above: joining the "ps" key blocks until the
	// in-flight call (if any) has finished, and every exec it started has
	// been waited on.
	t.Cleanup(func() {
		<-rt.listGroup.DoChan("ps", func() (any, error) { return nil, nil })
	})
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

func TestDockerRuntime_PullImage_ClassifiesLaunchFailures(t *testing.T) {
	cases := []struct {
		name      string
		image     string
		stderr    string
		wantCode  string
		wantTyped bool
	}{
		{
			name:      "missing tag",
			image:     "example.com/scion/agent:no-such-tag",
			stderr:    "Error response from daemon: manifest for example.com/scion/agent:no-such-tag not found: manifest unknown: manifest unknown",
			wantCode:  api.LaunchCodeImagePullFailed,
			wantTyped: true,
		},
		{
			name:      "unreachable registry",
			image:     "registry.invalid:5000/scion/agent:v1",
			stderr:    `Error response from daemon: Get "https://registry.invalid:5000/v2/": dial tcp: lookup registry.invalid: no such host`,
			wantCode:  api.LaunchCodeImagePullFailed,
			wantTyped: true,
		},
		{
			name:      "pull access denied",
			image:     "example.com/private/agent:v1",
			stderr:    "Error response from daemon: pull access denied for example.com/private/agent, repository does not exist or may require 'docker login': denied: requested access to the resource is denied",
			wantCode:  api.LaunchCodeImagePullFailed,
			wantTyped: true,
		},
		{
			name:      "malformed reference",
			image:     "INVALID::IMAGE",
			stderr:    "invalid reference format",
			wantCode:  api.LaunchCodeInvalidImage,
			wantTyped: true,
		},
		{
			name:      "uppercase repository reference",
			image:     "Example.com/UpperCase/Agent:v1",
			stderr:    "invalid reference format: repository name must be lowercase",
			wantCode:  api.LaunchCodeInvalidImage,
			wantTyped: true,
		},
		{
			name:      "whitespace in image reference",
			image:     "example.com/bad image:v1",
			stderr:    "invalid argument",
			wantCode:  api.LaunchCodeInvalidImage,
			wantTyped: true,
		},
		{
			name:      "daemon unreachable stays untyped",
			image:     "example.com/scion/agent:v1",
			stderr:    "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
			wantTyped: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			mockDocker := filepath.Join(tmpDir, "mock-docker")
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %s >&2\nexit 1\n", shellQuote(tc.stderr))
			if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
				t.Fatalf("failed to write mock docker: %v", err)
			}

			var steps []string
			ctx := WithLaunchProgress(context.Background(), func(step, message string) {
				steps = append(steps, step+": "+message)
			})

			rt := &DockerRuntime{Command: mockDocker}
			err := rt.PullImage(ctx, tc.image)
			if err == nil {
				t.Fatal("expected PullImage to fail, got nil")
			}

			wantProgress := api.LaunchStepImagePull + ": pulling the image"
			if len(steps) != 1 || steps[0] != wantProgress {
				t.Errorf("progress steps = %v, want [%q]", steps, wantProgress)
			}

			var lf *LaunchFailure
			if !tc.wantTyped {
				if errors.As(err, &lf) {
					t.Fatalf("expected untyped error, got LaunchFailure %+v", lf)
				}
				return
			}
			if !errors.As(err, &lf) {
				t.Fatalf("expected *LaunchFailure, got %T: %v", err, err)
			}
			if lf.Code != tc.wantCode {
				t.Errorf("LaunchFailure.Code = %q, want %q", lf.Code, tc.wantCode)
			}
			if lf.Step != api.LaunchStepImagePull {
				t.Errorf("LaunchFailure.Step = %q, want %q", lf.Step, api.LaunchStepImagePull)
			}
			if lf.Reason != "" {
				t.Errorf("LaunchFailure.Reason = %q, want empty (must not leak free text)", lf.Reason)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("pull %q:", tc.image)) {
				t.Errorf("error text %q lost pull prefix", err.Error())
			}
		})
	}

	t.Run("successful pull reports step", func(t *testing.T) {
		tmpDir := t.TempDir()
		mockDocker := filepath.Join(tmpDir, "mock-docker")
		if err := os.WriteFile(mockDocker, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatalf("failed to write mock docker: %v", err)
		}
		var steps []string
		ctx := WithLaunchProgress(context.Background(), func(step, message string) {
			steps = append(steps, step+": "+message)
		})
		rt := &DockerRuntime{Command: mockDocker}
		if err := rt.PullImage(ctx, "example.com/scion/agent:v1"); err != nil {
			t.Fatalf("PullImage: %v", err)
		}
		wantProgress := api.LaunchStepImagePull + ": pulling the image"
		if len(steps) != 1 || steps[0] != wantProgress {
			t.Errorf("progress steps = %v, want [%q]", steps, wantProgress)
		}
	})

	t.Run("missing binary stays untyped", func(t *testing.T) {
		rt := &DockerRuntime{Command: filepath.Join(t.TempDir(), "no-such-docker")}
		err := rt.PullImage(context.Background(), "example.com/scion/agent:v1")
		if err == nil {
			t.Fatal("expected error for missing binary")
		}
		var lf *LaunchFailure
		if errors.As(err, &lf) {
			t.Fatalf("expected untyped error for missing binary, got %+v", lf)
		}
	})
}

func TestDockerRuntime_Run_ClassifiesImageFailuresAndReportsPullStep(t *testing.T) {
	cases := []struct {
		name      string
		image     string
		stderr    string
		wantCode  string
		wantStep  string
		wantTyped bool
	}{
		{
			name:      "missing tag during run",
			image:     "example.com/scion/agent:missing",
			stderr:    "Unable to find image 'example.com/scion/agent:missing' locally\ndocker: Error response from daemon: manifest for example.com/scion/agent:missing not found: manifest unknown: manifest unknown.",
			wantCode:  api.LaunchCodeImagePullFailed,
			wantStep:  api.LaunchStepImagePull,
			wantTyped: true,
		},
		{
			name:      "malformed reference during run",
			image:     "INVALID::IMAGE",
			stderr:    "docker: invalid reference format.",
			wantCode:  api.LaunchCodeInvalidImage,
			wantStep:  api.LaunchStepImagePull,
			wantTyped: true,
		},
		{
			name:      "non-image container failure stays untyped",
			image:     "example.com/scion/agent:v1",
			stderr:    "docker: Error response from daemon: invalid mount config for type \"bind\": bind source path does not exist.",
			wantTyped: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			mockDocker := filepath.Join(tmpDir, "mock-docker")
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %s >&2\nexit 125\n", shellQuote(tc.stderr))
			if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
				t.Fatalf("failed to write mock docker: %v", err)
			}

			var steps []string
			rt := &DockerRuntime{Command: mockDocker}
			cfg := RunConfig{
				Harness:      &harness.Generic{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        tc.image,
				Task:         "hello",
				Progress: func(step, message string) {
					steps = append(steps, step+": "+message)
				},
			}

			_, err := rt.Run(context.Background(), cfg)
			if err == nil {
				t.Fatal("expected Run to fail, got nil")
			}

			var lf *LaunchFailure
			if !tc.wantTyped {
				if errors.As(err, &lf) {
					t.Fatalf("expected untyped error, got LaunchFailure %+v", lf)
				}
				if len(steps) != 0 {
					t.Fatalf("expected no image_pull progress step, got %v", steps)
				}
				return
			}
			if !errors.As(err, &lf) {
				t.Fatalf("expected *LaunchFailure, got %T: %v", err, err)
			}
			if lf.Code != tc.wantCode {
				t.Errorf("LaunchFailure.Code = %q, want %q", lf.Code, tc.wantCode)
			}
			if lf.Step != tc.wantStep {
				t.Errorf("LaunchFailure.Step = %q, want %q", lf.Step, tc.wantStep)
			}
			wantProgress := api.LaunchStepImagePull + ": pulling the image"
			if len(steps) != 1 || steps[0] != wantProgress {
				t.Errorf("progress steps = %v, want [%q]", steps, wantProgress)
			}
		})
	}
}
