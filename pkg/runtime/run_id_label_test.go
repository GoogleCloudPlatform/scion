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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// The scion.run_id label round-trips on Docker, Podman and Apple
// (ptone/scion#2550 P1): Run writes it as a container label, and List
// reports it as AgentInfo.RunID.

// fakeContainerCLI writes a fake container CLI that logs the arguments of
// "run" (and answers with an ID) and prints listOutput for "list"/"ps".
func fakeContainerCLI(t *testing.T, listOutput string) (cmd, runLog string) {
	t.Helper()
	dir := t.TempDir()
	cmd = filepath.Join(dir, "fake-cli")
	runLog = filepath.Join(dir, "run.log")
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
  run) echo "$@" >> %q; echo cid-123 ;;
  ps|list) echo '%s' ;;
esac
`, runLog, listOutput)
	if err := os.WriteFile(cmd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return cmd, runLog
}

func runWithRunLabel(t *testing.T, rt Runtime, runLog string) {
	t.Helper()
	cfg := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "run-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Task:         "hello",
		Labels:       map[string]string{api.LabelRunID: "run-1"},
	}
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("%s Run: %v", rt.Name(), err)
	}
	data, err := os.ReadFile(runLog)
	if err != nil {
		t.Fatalf("%s: run not invoked: %v", rt.Name(), err)
	}
	if !strings.Contains(string(data), "--label "+api.LabelRunID+"=run-1") {
		t.Errorf("%s run args lack the run label: %s", rt.Name(), data)
	}
}

func assertListedRunID(t *testing.T, rt Runtime, want string) {
	t.Helper()
	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("%s List: %v", rt.Name(), err)
	}
	if len(agents) != 1 {
		t.Fatalf("%s List: got %d agents, want 1", rt.Name(), len(agents))
	}
	if agents[0].RunID != want {
		t.Errorf("%s List RunID = %q, want %q", rt.Name(), agents[0].RunID, want)
	}
}

func TestDockerRuntime_RunIDLabelRoundTrip(t *testing.T) {
	cmd, runLog := fakeContainerCLI(t,
		`{"ID":"cid-123","Names":"run-agent","Status":"Up","Image":"img","Labels":"scion.name=run-agent,scion.run_id=run-1"}`)
	rt := &DockerRuntime{Command: cmd}
	runWithRunLabel(t, rt, runLog)
	assertListedRunID(t, rt, "run-1")
}

func TestPodmanRuntime_RunIDLabelRoundTrip(t *testing.T) {
	cmd, runLog := fakeContainerCLI(t,
		`[{"Id":"cid-123","Names":["run-agent"],"Status":"Up","Image":"img","Labels":{"scion.name":"run-agent","scion.run_id":"run-1"}}]`)
	rt := &PodmanRuntime{Command: cmd}
	runWithRunLabel(t, rt, runLog)
	assertListedRunID(t, rt, "run-1")
}

func TestAppleContainerRuntime_RunIDLabelRoundTrip(t *testing.T) {
	cmd, runLog := fakeContainerCLI(t,
		`[{"status":"running","configuration":{"id":"run-agent","labels":{"scion.name":"run-agent","scion.run_id":"run-1"},"image":{"reference":"img"}}}]`)
	rt := &AppleContainerRuntime{Command: cmd}
	runWithRunLabel(t, rt, runLog)
	assertListedRunID(t, rt, "run-1")
}

// A legacy container without the label lists with an empty RunID.
func TestDockerRuntime_ListLegacyContainerHasNoRunID(t *testing.T) {
	cmd, _ := fakeContainerCLI(t,
		`{"ID":"cid-0","Names":"old-agent","Status":"Up","Image":"img","Labels":"scion.name=old-agent"}`)
	assertListedRunID(t, &DockerRuntime{Command: cmd}, "")
}
