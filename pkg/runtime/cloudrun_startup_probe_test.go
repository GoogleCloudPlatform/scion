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
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/startuplistener"
)

// TestBuildCloudRunInstance_StartupProbeOnStartupListener checks that the
// built instance replaces the platform's default TCP startup probe (on 8080,
// where nothing listens) with an explicit TCP probe on sciontool's startup
// listener port, declares that port, and tells sciontool init to open the
// listener on the same port.
func TestBuildCloudRunInstance_StartupProbeOnStartupListener(t *testing.T) {
	rt, err := NewCloudRunRuntime(&config.CloudRunConfig{ProjectID: "test-project", Location: "us-central1"})
	if err != nil {
		t.Fatal(err)
	}
	inst := rt.buildCloudRunInstance(RunConfig{
		Image:  "test-image:latest",
		Labels: map[string]string{"agent_id": "agent-1"},
	}, 1000, 1000, nil)
	if len(inst.GetContainers()) != 1 {
		t.Fatalf("got %d containers, want 1", len(inst.GetContainers()))
	}
	c := inst.GetContainers()[0]

	ports := c.GetPorts()
	if len(ports) != 1 || ports[0].GetContainerPort() != startuplistener.Port {
		t.Errorf("container ports = %v, want exactly [%d]", ports, startuplistener.Port)
	}

	probe := c.GetStartupProbe()
	tcp := probe.GetTcpSocket()
	if tcp == nil {
		t.Fatalf("startup probe = %v, want a TCP socket probe", probe)
	}
	if tcp.GetPort() != startuplistener.Port {
		t.Errorf("startup probe port = %d, want %d (the startup listener port)", tcp.GetPort(), startuplistener.Port)
	}
	// Startup probe limits from the Probe fields: period 1..240, threshold >= 1.
	if probe.GetPeriodSeconds() != 10 || probe.GetFailureThreshold() != 24 {
		t.Errorf("startup probe period/threshold = %d/%d, want 10/24", probe.GetPeriodSeconds(), probe.GetFailureThreshold())
	}

	var got []string
	for _, ev := range c.GetEnv() {
		if ev.GetName() == startuplistener.EnvPort {
			got = append(got, ev.GetValue())
		}
	}
	if want := strconv.Itoa(startuplistener.Port); len(got) != 1 || got[0] != want {
		t.Errorf("%s values = %v, want exactly [%s]", startuplistener.EnvPort, got, want)
	}
}

// TestBuildCommonRunArgs_NoStartupListenerEnv checks that the container
// runtimes built on buildCommonRunArgs (Docker, Podman, Apple) never ask
// sciontool init to open the startup listener.
func TestBuildCommonRunArgs_NoStartupListenerEnv(t *testing.T) {
	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}
	for _, arg := range args {
		if strings.Contains(arg, startuplistener.EnvPort) {
			t.Errorf("run args carry %q; only the cloudrun Instances runtime sets %s", arg, startuplistener.EnvPort)
		}
	}
}
