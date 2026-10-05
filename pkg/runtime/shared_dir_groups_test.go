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
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#3155: nfs shared-dir leaf groups as pod
// supplementalGroups (Kubernetes) and --group-add (Docker/Podman).

func sharedDirGroupsRunConfig(groups ...int64) RunConfig {
	return RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		Image:        "test-image",
		UnixUsername: "scion",
		Task:         "hello",
		Labels:       map[string]string{"scion.project": "myproject"},
		SharedDirs:   []api.SharedDir{{Name: "scratchpad"}},
		SharedDirStorage: &SharedDirRealization{
			Backend:            "nfs",
			PVClaimName:        "scion-shared",
			SubPaths:           map[string]string{"scratchpad": "projects/pid-1/shared-dirs/scratchpad"},
			SupplementalGroups: groups,
		},
	}
}

func TestBuildPod_SharedDirLeafGroups_InSupplementalGroups(t *testing.T) {
	fsGroup := int64(os.Getgid()) // local workspace backend: fsGroup is the broker gid
	leaf := int64(4242)
	require.NotEqual(t, fsGroup, leaf)

	pod, err := newNFSTestK8sRuntime().buildPod("default", sharedDirGroupsRunConfig(leaf))
	require.NoError(t, err)
	sc := pod.Spec.SecurityContext
	require.NotNil(t, sc)
	assert.Equal(t, []int64{leaf}, sc.SupplementalGroups)
	require.NotNil(t, sc.FSGroup)
	assert.Equal(t, fsGroup, *sc.FSGroup, "fsGroup must not change")
}

func TestBuildPod_SharedDirLeafGroups_FSGroupSkipped(t *testing.T) {
	fsGroup := int64(os.Getgid())
	pod, err := newNFSTestK8sRuntime().buildPod("default", sharedDirGroupsRunConfig(fsGroup, 4242))
	require.NoError(t, err)
	assert.Equal(t, []int64{4242}, pod.Spec.SecurityContext.SupplementalGroups,
		"a leaf gid equal to fsGroup is already a pod group and must not be repeated")
}

func TestBuildPod_NoNFSSharedDirs_NoSupplementalGroups(t *testing.T) {
	rt := newNFSTestK8sRuntime()

	// No shared dirs at all.
	cfg := sharedDirGroupsRunConfig(4242)
	cfg.SharedDirs = nil
	cfg.SharedDirStorage = nil
	pod, err := rt.buildPod("default", cfg)
	require.NoError(t, err)
	assert.Nil(t, pod.Spec.SecurityContext.SupplementalGroups)

	// nfs realization without leaf groups (none passed the guard).
	pod, err = rt.buildPod("default", sharedDirGroupsRunConfig())
	require.NoError(t, err)
	assert.Nil(t, pod.Spec.SecurityContext.SupplementalGroups)

	// nfs realization with groups but no shared dirs mounted.
	cfg = sharedDirGroupsRunConfig(4242)
	cfg.SharedDirs = nil
	pod, err = rt.buildPod("default", cfg)
	require.NoError(t, err)
	assert.Nil(t, pod.Spec.SecurityContext.SupplementalGroups)
}

func TestSharedDirSupplementalGroups(t *testing.T) {
	assert.Nil(t, sharedDirSupplementalGroups(RunConfig{}, 1000))
	cfg := sharedDirGroupsRunConfig(1000, 0, -5, 1500)
	assert.Equal(t, []int64{1500}, sharedDirSupplementalGroups(cfg, 1000))
	cfg.SharedDirStorage.Backend = "local"
	assert.Nil(t, sharedDirSupplementalGroups(cfg, 1000))
}

// The broker never adds the sciontool env var to the pod env. It would have
// no effect there anyway: pods run non-root, so sciontool does no privilege
// drop and the process keeps the pod's supplementalGroups.
func TestBuildPod_SharedDirLeafGroups_NoSupplementalGIDsEnv(t *testing.T) {
	pod, err := newNFSTestK8sRuntime().buildPod("default", sharedDirGroupsRunConfig(4242))
	require.NoError(t, err)
	for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		for _, e := range c.Env {
			assert.NotEqual(t, SupplementalGIDsEnvVar, e.Name, "container %s", c.Name)
		}
	}
}

func writeEchoCommand(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mock-cli")
	require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\necho \"$@\"\n"), 0o755))
	return p
}

// argsBeforeImage returns the run args up to (not including) the image, so
// flag assertions cannot match the container command.
func argsBeforeImage(t *testing.T, out, image string) []string {
	t.Helper()
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == image {
			return fields[:i]
		}
	}
	t.Fatalf("image %q not in %q", image, out)
	return nil
}

func countEnv(args []string, name string) (n int, last string) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-e" && strings.HasPrefix(args[i+1], name+"=") {
			n++
			last = strings.TrimPrefix(args[i+1], name+"=")
		}
	}
	return n, last
}

func TestDockerRun_SharedDirLeafGroups_GroupAdd(t *testing.T) {
	rt := &DockerRuntime{Command: writeEchoCommand(t)}
	cfg := sharedDirGroupsRunConfig(4242, 4343)
	// A template or user value must be replaced by the broker's.
	cfg.Env = []string{SupplementalGIDsEnvVar + "=0,27"}
	cfg.ResolvedSecrets = []api.ResolvedSecret{{Name: "s", Type: "environment", Target: SupplementalGIDsEnvVar, Value: "0"}}

	out, err := rt.Run(context.Background(), cfg)
	require.NoError(t, err)
	args := argsBeforeImage(t, out, cfg.Image)
	assert.Contains(t, strings.Join(args, " "), "--group-add 4242 --group-add 4343")
	n, v := countEnv(args, SupplementalGIDsEnvVar)
	assert.Equal(t, 1, n, "exactly one broker-set value")
	assert.Equal(t, "4242,4343", v)
}

func TestDockerRun_NoNFSSharedDirs_NoGroupAdd(t *testing.T) {
	rt := &DockerRuntime{Command: writeEchoCommand(t)}
	noStorage := sharedDirGroupsRunConfig(4242)
	noStorage.SharedDirStorage = nil
	noStorage.Env = []string{SupplementalGIDsEnvVar + "=27"}
	noDirs := sharedDirGroupsRunConfig(4242) // groups set, but no shared dirs mounted
	noDirs.SharedDirs = nil

	for name, cfg := range map[string]RunConfig{"no nfs storage": noStorage, "no shared dirs": noDirs} {
		t.Run(name, func(t *testing.T) {
			out, err := rt.Run(context.Background(), cfg)
			require.NoError(t, err)
			args := argsBeforeImage(t, out, cfg.Image)
			assert.NotContains(t, args, "--group-add")
			n, _ := countEnv(args, SupplementalGIDsEnvVar)
			assert.Zero(t, n, "a user value is dropped even when the broker sets none")
		})
	}
}

func TestPodmanRun_SharedDirLeafGroups(t *testing.T) {
	cmd := writeEchoCommand(t)

	out, err := (&PodmanRuntime{Command: cmd}).Run(context.Background(), sharedDirGroupsRunConfig(4242))
	require.NoError(t, err)
	args := argsBeforeImage(t, out, "test-image")
	assert.Contains(t, strings.Join(args, " "), "--group-add 4242")

	// Rootless: the leaf gid is not mapped in the user namespace; warn and skip.
	logs := &bytes.Buffer{}
	prev := runtimeLog
	runtimeLog = slog.New(slog.NewTextHandler(logs, nil))
	t.Cleanup(func() { runtimeLog = prev })
	out, err = (&PodmanRuntime{Command: cmd, Rootless: true}).Run(context.Background(), sharedDirGroupsRunConfig(4242))
	require.NoError(t, err)
	args = argsBeforeImage(t, out, "test-image")
	assert.NotContains(t, args, "--group-add")
	n, _ := countEnv(args, SupplementalGIDsEnvVar)
	assert.Zero(t, n)
	assert.Contains(t, logs.String(), "shared dir groups are not supported")
	assert.Contains(t, logs.String(), "rootless")
}

// An environment-type secret cannot set the broker-owned variable either.
func TestDockerRun_SharedDirLeafGroups_SecretValueReplaced(t *testing.T) {
	rt := &DockerRuntime{Command: writeEchoCommand(t)}
	cfg := sharedDirGroupsRunConfig(4242)
	cfg.ResolvedSecrets = []api.ResolvedSecret{{Name: "s", Type: "environment", Target: SupplementalGIDsEnvVar, Value: "0,27"}}

	out, err := rt.Run(context.Background(), cfg)
	require.NoError(t, err)
	n, v := countEnv(argsBeforeImage(t, out, cfg.Image), SupplementalGIDsEnvVar)
	assert.Equal(t, 1, n)
	assert.Equal(t, "4242", v)
}

// Apple's container CLI has no --group-add: warn and start unchanged.
func TestAppleRun_SharedDirLeafGroups_Skipped(t *testing.T) {
	logs := &bytes.Buffer{}
	prev := runtimeLog
	runtimeLog = slog.New(slog.NewTextHandler(logs, nil))
	t.Cleanup(func() { runtimeLog = prev })

	out, err := (&AppleContainerRuntime{Command: writeEchoCommand(t)}).Run(context.Background(), sharedDirGroupsRunConfig(4242))
	require.NoError(t, err)
	args := argsBeforeImage(t, out, "test-image")
	assert.NotContains(t, args, "--group-add")
	n, _ := countEnv(args, SupplementalGIDsEnvVar)
	assert.Zero(t, n)
	assert.Contains(t, logs.String(), "shared dir groups are not supported")
	assert.Contains(t, logs.String(), "runtime=container")
}
