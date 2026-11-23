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
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func hasCapFlag(args []string, flag, capName string) bool {
	prefix := flag + "="
	for i := 0; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) && strings.EqualFold(args[i+1], capName) {
			return true
		}
		if strings.HasPrefix(args[i], prefix) && strings.EqualFold(args[i][len(prefix):], capName) {
			return true
		}
	}
	return false
}

func hasAnyFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// TestBuildCommonRunArgs_AgentCapabilities verifies that Docker and Podman
// agent containers unconditionally drop CAP_NET_RAW across every metadata mode
// and network mode, never grant CAP_NET_ADMIN, and never re-add CAP_NET_RAW
// (even when FUSE mounts add SYS_ADMIN), while Apple Container argv receives no
// --cap-drop flags.
func TestBuildCommonRunArgs_AgentCapabilities(t *testing.T) {
	metadataModes := []struct {
		name         string
		mode         string
		interception bool
	}{
		{name: "block", mode: "block", interception: true},
		{name: "assign", mode: "assign", interception: true},
		{name: "passthrough", mode: "passthrough", interception: false},
		{name: "unset", mode: "", interception: false},
	}
	networkModes := []struct {
		name string
		mode string
	}{
		{name: "default", mode: ""},
		{name: "host", mode: "host"},
	}
	runtimes := []struct {
		name        string
		wantDropRaw bool
	}{
		{name: "docker", wantDropRaw: true},
		{name: "podman", wantDropRaw: true},
		{name: appleContainerRuntimeName, wantDropRaw: false},
	}

	for _, rt := range runtimes {
		for _, mm := range metadataModes {
			for _, nm := range networkModes {
				for _, withFuse := range []bool{false, true} {
					name := fmt.Sprintf("%s/%s/net=%s/fuse=%v", rt.name, mm.name, nm.name, withFuse)
					t.Run(name, func(t *testing.T) {
						cfg := minimalRunConfig()
						cfg.RuntimeName = rt.name
						cfg.NetworkMode = nm.mode
						cfg.MetadataInterception = mm.interception && nm.mode != "host"
						if mm.mode != "" {
							cfg.Env = append(cfg.Env, "SCION_METADATA_MODE="+mm.mode)
						}
						if withFuse {
							cfg.Volumes = []api.VolumeMount{
								{Type: "gcs", Bucket: "test-bucket", Target: "/gcs"},
							}
						}

						args, err := buildCommonRunArgs(cfg)
						if err != nil {
							t.Fatalf("buildCommonRunArgs: %v", err)
						}

						gotDropRaw := hasCapFlag(args, "--cap-drop", "NET_RAW")
						if gotDropRaw != rt.wantDropRaw {
							t.Errorf("--cap-drop NET_RAW present = %v, want %v (args: %v)", gotDropRaw, rt.wantDropRaw, args)
						}
						if !rt.wantDropRaw && hasAnyFlag(args, "--cap-drop") {
							t.Errorf("runtime %q must not emit --cap-drop (args: %v)", rt.name, args)
						}
						if hasCapFlag(args, "--cap-add", "NET_ADMIN") {
							t.Errorf("agent container must never receive --cap-add NET_ADMIN (args: %v)", args)
						}
						if hasCapFlag(args, "--cap-add", "NET_RAW") {
							t.Errorf("agent container must never receive --cap-add NET_RAW (args: %v)", args)
						}
						if hasCapFlag(args, "--cap-add", "ALL") {
							t.Errorf("agent container must never receive --cap-add ALL (args: %v)", args)
						}
						if hasAnyFlag(args, "--privileged") {
							t.Errorf("agent container must never receive --privileged (args: %v)", args)
						}
					})
				}
			}
		}
	}
}

// TestRuntimeAdapters_AgentCapDropNetRaw verifies the full Run argv emitted by
// DockerRuntime, PodmanRuntime (rootful and rootless), and
// AppleContainerRuntime across metadata modes and network modes.
func TestRuntimeAdapters_AgentCapDropNetRaw(t *testing.T) {
	dir := t.TempDir()

	adapters := []struct {
		name        string
		newRT       func(cmd string) Runtime
		wantDropRaw bool
	}{
		{name: "docker", newRT: func(cmd string) Runtime { return &DockerRuntime{Command: cmd} }, wantDropRaw: true},
		{name: "podman-rootful", newRT: func(cmd string) Runtime { return &PodmanRuntime{Command: cmd, Rootless: false} }, wantDropRaw: true},
		{name: "podman-rootless", newRT: func(cmd string) Runtime { return &PodmanRuntime{Command: cmd, Rootless: true} }, wantDropRaw: true},
		{name: "apple", newRT: func(cmd string) Runtime { return &AppleContainerRuntime{Command: cmd} }, wantDropRaw: false},
	}

	modes := []struct {
		name         string
		metaMode     string
		interception bool
		netMode      string
	}{
		{name: "block/default", metaMode: "block", interception: true, netMode: ""},
		{name: "assign/default", metaMode: "assign", interception: true, netMode: ""},
		{name: "passthrough/default", metaMode: "passthrough", interception: false, netMode: ""},
		{name: "unset/default", metaMode: "", interception: false, netMode: ""},
		{name: "block/host", metaMode: "block", interception: false, netMode: "host"},
		{name: "passthrough/host", metaMode: "passthrough", interception: false, netMode: "host"},
	}

	for _, ad := range adapters {
		for _, tc := range modes {
			t.Run(ad.name+"/"+tc.name, func(t *testing.T) {
				cfg := RunConfig{
					Harness:              &harness.Generic{},
					Name:                 "test-agent",
					UnixUsername:         "scion",
					Image:                "scion-agent:latest",
					Task:                 "hello",
					NetworkMode:          tc.netMode,
					MetadataInterception: tc.interception,
				}
				if tc.metaMode != "" {
					cfg.Env = []string{"SCION_METADATA_MODE=" + tc.metaMode}
				}
				// For interception=true, the first "run" is the agent container and
				// subsequent "run" invocations are netns-setup; record only the first.
				firstArgvLog := filepath.Join(dir, fmt.Sprintf("argv-%s-%s", ad.name, strings.ReplaceAll(tc.name, "/", "-")))
				firstMock := filepath.Join(dir, fmt.Sprintf("mock-%s-%s", ad.name, strings.ReplaceAll(tc.name, "/", "-")))
				firstScript := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"run\" ] && [ ! -f %q ]; then printf '%%s\\n' \"$@\" > %q; fi\nif [ \"$1\" = \"run\" ]; then echo %s; fi\n", firstArgvLog, firstArgvLog, mockPodmanID)
				if err := os.WriteFile(firstMock, []byte(firstScript), 0o755); err != nil {
					t.Fatal(err)
				}
				rt := ad.newRT(firstMock)

				if _, err := rt.Run(context.Background(), cfg); err != nil {
					t.Fatalf("Run: %v", err)
				}
				raw, err := os.ReadFile(firstArgvLog)
				if err != nil {
					t.Fatalf("read recorded argv: %v", err)
				}
				args := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")

				gotDropRaw := hasCapFlag(args, "--cap-drop", "NET_RAW")
				if gotDropRaw != ad.wantDropRaw {
					t.Errorf("--cap-drop NET_RAW present = %v, want %v (args: %v)", gotDropRaw, ad.wantDropRaw, args)
				}
				if !ad.wantDropRaw && hasAnyFlag(args, "--cap-drop") {
					t.Errorf("%s must not emit --cap-drop (args: %v)", ad.name, args)
				}
				if hasCapFlag(args, "--cap-add", "NET_ADMIN") {
					t.Errorf("agent container must never receive --cap-add NET_ADMIN (args: %v)", args)
				}
				if hasCapFlag(args, "--cap-add", "NET_RAW") {
					t.Errorf("agent container must never receive --cap-add NET_RAW (args: %v)", args)
				}
				if hasCapFlag(args, "--cap-add", "ALL") {
					t.Errorf("agent container must never receive --cap-add ALL (args: %v)", args)
				}
				if hasAnyFlag(args, "--privileged") {
					t.Errorf("agent container must never receive --privileged (args: %v)", args)
				}
			})
		}
	}
}

// TestKubernetesRuntime_AgentCapabilitiesUnchanged verifies that the Kubernetes
// pod builder continues to drop ALL capabilities without adding NET_RAW or
// NET_ADMIN.
func TestKubernetesRuntime_AgentCapabilitiesUnchanged(t *testing.T) {
	rt := &KubernetesRuntime{DefaultNamespace: "default"}
	pod, err := rt.buildPod("default", RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Task:         "hello",
	})
	if err != nil {
		t.Fatalf("buildPod: %v", err)
	}
	if len(pod.Spec.Containers) == 0 {
		t.Fatal("pod has no containers")
	}
	sc := pod.Spec.Containers[0].SecurityContext
	if sc == nil || sc.Capabilities == nil {
		t.Fatal("agent container missing SecurityContext.Capabilities")
	}
	if !slices.Contains(sc.Capabilities.Drop, "ALL") {
		t.Errorf("Capabilities.Drop = %v, want to contain ALL", sc.Capabilities.Drop)
	}
	if len(sc.Capabilities.Add) != 0 {
		t.Errorf("Capabilities.Add = %v, want empty", sc.Capabilities.Add)
	}
}
