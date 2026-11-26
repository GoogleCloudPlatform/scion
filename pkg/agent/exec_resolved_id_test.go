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

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const resolvedTestContainerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type containerFakeSpec struct {
	name    string
	newRT   func(cli string) runtime.Runtime
	listing string
}

func containerFakeSpecs() []containerFakeSpec {
	return []containerFakeSpec{
		{
			name:    "docker",
			newRT:   func(cli string) runtime.Runtime { return &runtime.DockerRuntime{Command: cli} },
			listing: `{"ID":"` + resolvedTestContainerID + `","Names":"proj--worker","Status":"Up 1 minute","Image":"img","Labels":"scion.name=worker,scion.project_id=proj-1,agent_id=agent-1"}` + "\n",
		},
		{
			name:    "podman",
			newRT:   func(cli string) runtime.Runtime { return &runtime.PodmanRuntime{Command: cli} },
			listing: `[{"Id":"` + resolvedTestContainerID + `","Names":["proj--worker"],"Status":"running","Image":"img","Labels":{"scion.name":"worker","scion.project_id":"proj-1","agent_id":"agent-1"}}]`,
		},
		{
			name:    "apple",
			newRT:   func(cli string) runtime.Runtime { return &runtime.AppleContainerRuntime{Command: cli} },
			listing: `[{"status":"running","configuration":{"id":"` + resolvedTestContainerID + `","labels":{"scion.name":"worker","scion.project_id":"proj-1","agent_id":"agent-1"},"image":{"reference":"img"}}}]`,
		},
	}
}

// writeCountingContainerCLI writes a fake docker/podman/apple CLI that logs
// every list ("ps"/"list") and "exec" invocation to logPath. On exec, it
// responds to the harness readiness probe with a settled ready hook output and
// to "tmux -V" with a supported version, draining stdin for ExecWithStdin.
func writeCountingContainerCLI(t *testing.T, listing string) (cliPath, logPath string) {
	t.Helper()
	dir := t.TempDir()
	listingPath := filepath.Join(dir, "listing")
	probePath := filepath.Join(dir, "probe-out")
	logPath = filepath.Join(dir, "calls.log")
	if err := os.WriteFile(listingPath, []byte(listing), 0o644); err != nil {
		t.Fatal(err)
	}
	probeOut := hookProbe(true)
	if err := os.WriteFile(probePath, []byte(probeOut), 0o644); err != nil {
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
    printf 'exec\n' >> %q
    case "$*" in
      *"tmux -V"*)
        printf 'tmux 3.4\n'
        exit 0
        ;;
      *"sh -c"*)
        cat %q
        exit 0
        ;;
    esac
    cat >/dev/null
    exit 0
    ;;
esac
exit 1
`, logPath, listingPath, logPath, probePath)
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cliPath, logPath
}

func countContainerCalls(t *testing.T, logPath string) (lists, execs int) {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading call log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		switch line {
		case "":
		case "list":
			lists++
		case "exec":
			execs++
		default:
			t.Fatalf("unexpected call log line: %q", line)
		}
	}
	return lists, execs
}

// TestDeliveryFlush_SkipsInnerListOnContainerRuntimes verifies that a buffered
// delivery flush through AgentManager on Docker, Podman and Apple Container
// runs exactly 3 List calls (checkDeliveryTarget, waitHarnessReady,
// findMessageTargetCounted) + 9 exec calls (1 readiness probe + 8 tmuxdeliver
// steps), with 0 inner List calls inside Exec/ExecWithStdin.
func TestDeliveryFlush_SkipsInnerListOnContainerRuntimes(t *testing.T) {
	for _, spec := range containerFakeSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			cli, logPath := writeCountingContainerCLI(t, spec.listing)
			rt := spec.newRT(cli)

			mgr := NewManager(rt).(*AgentManager)
			mgr.readyTimings = fastReadyTimings
			// Replace only the debounce delay so the flush runs immediately.
			mgr.msgBuffer.Close()
			mgr.msgBuffer = NewMessageBuffer(20*time.Millisecond, func(agentID, projectID string, message string, interrupt bool) error {
				return mgr.deliverImmediate(context.Background(), agentID, projectID, message, interrupt)
			})
			mgr.msgBuffer.deliverFlush = func(f bufferedFlush) error {
				return mgr.deliver(context.Background(), deliveryRequest{
					agentID: f.agentID, projectID: f.projectID, message: f.text,
					messageIDs: f.messageIDs, partialIDs: f.partialIDs, readyWanted: f.readyWanted,
					hasAttachments: f.hasAttachments, parts: f.parts, onSuccess: f.onSuccess,
				})
			}
			mgr.msgBuffer.waitReady = mgr.waitHarnessReady
			defer mgr.Close()

			done := make(chan struct{}, 1)
			ctx := WithDeliverySuccessHandler(context.Background(), func() {
				select {
				case done <- struct{}{}:
				default:
				}
			})
			if err := mgr.Message(ctx, "worker", "proj-1", "hello", false); err != nil {
				t.Fatalf("Message: %v", err)
			}

			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("timed out waiting for delivery flush")
			}

			lists, execs := countContainerCalls(t, logPath)
			if lists != 3 || execs != 9 {
				t.Fatalf("%s delivery flush counts: lists=%d execs=%d, want lists=3 execs=9", spec.name, lists, execs)
			}
		})
	}
}

// TestSendKeys_SkipsInnerListOnContainerRuntimes verifies that SendKeys on
// Docker, Podman and Apple Container runs only its 2 explicit resolveKeysTarget
// List calls and 0 inner List calls during tmux -V, has-session, and
// source-file - (3 execs total).
func TestSendKeys_SkipsInnerListOnContainerRuntimes(t *testing.T) {
	for _, spec := range containerFakeSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			cli, logPath := writeCountingContainerCLI(t, spec.listing)
			rt := spec.newRT(cli)

			mgr := NewManager(rt).(*AgentManager)
			defer mgr.Close()

			if err := mgr.SendKeys(context.Background(), "proj-1", "worker", "agent-1", "Enter"); err != nil {
				t.Fatalf("SendKeys: %v", err)
			}

			lists, execs := countContainerCalls(t, logPath)
			if lists != 2 || execs != 3 {
				t.Fatalf("%s SendKeys counts: lists=%d execs=%d, want lists=2 execs=3", spec.name, lists, execs)
			}
		})
	}
}
