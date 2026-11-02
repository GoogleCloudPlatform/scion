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
	"strings"
	"testing"
)

// scriptLocality points the locality checks at a scripted environment and
// command runner for the test.
func scriptLocality(t *testing.T, env map[string]string, out string, runErr error) *[][]string {
	t.Helper()
	prevEnv, prevCmd := daemonLocalityEnv, daemonLocalityCommand
	var calls [][]string
	daemonLocalityEnv = func(k string) string { return env[k] }
	daemonLocalityCommand = func(_ context.Context, command string, args ...string) (string, error) {
		calls = append(calls, append([]string{command}, args...))
		return out, runErr
	}
	t.Cleanup(func() { daemonLocalityEnv, daemonLocalityCommand = prevEnv, prevCmd })
	return &calls
}

func TestEndpointIsLocalSocket(t *testing.T) {
	for ep, want := range map[string]bool{
		"unix:///var/run/docker.sock":             true,
		"unix:///Users/u/.docker/run/docker.sock": true,
		"unix://relative.sock":                    false,
		"tcp://10.0.0.5:2376":                     false,
		"ssh://user@builder":                      false,
		"https://daemon.example:2376":             false,
		"npipe:////./pipe/docker_engine":          false,
		"/var/run/docker.sock":                    false,
		"":                                        false,
		"::not a url":                             false,
	} {
		if got := endpointIsLocalSocket(ep); got != want {
			t.Errorf("endpointIsLocalSocket(%q) = %v, want %v", ep, got, want)
		}
	}
}

func TestDockerDaemonLocal(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		out     string
		runErr  error
		want    bool
		wantErr bool
		wantRun bool
	}{
		{"DOCKER_HOST local socket", map[string]string{"DOCKER_HOST": "unix:///var/run/docker.sock"}, "", nil, true, false, false},
		{"DOCKER_HOST tcp", map[string]string{"DOCKER_HOST": "tcp://10.0.0.5:2376"}, "", nil, false, false, false},
		{"DOCKER_HOST ssh", map[string]string{"DOCKER_HOST": "ssh://u@h"}, "", nil, false, false, false},
		{"context local socket", nil, "unix:///var/run/docker.sock\n", nil, true, false, true},
		{"context ssh (remote via docker context)", map[string]string{"DOCKER_CONTEXT": "builder"}, "ssh://u@builder\n", nil, false, false, true},
		{"context tcp", nil, "tcp://10.0.0.5:2376", nil, false, false, true},
		{"context unresolvable", nil, "", errors.New("context \"x\" does not exist"), false, true, true},
		{"context empty", nil, "  \n", nil, false, true, true},
		{"context multi-line", nil, "unix:///a.sock\nunix:///b.sock\n", nil, false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := scriptLocality(t, tc.env, tc.out, tc.runErr)
			got, err := dockerDaemonLocal(context.Background(), "docker")
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got (%v, %v), want (%v, err=%v)", got, err, tc.want, tc.wantErr)
			}
			if ran := len(*calls) > 0; ran != tc.wantRun {
				t.Fatalf("docker context inspect ran = %v, want %v", ran, tc.wantRun)
			}
			// The current context is resolved by the CLI: no context name
			// is passed.
			want := []string{"docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}"}
			for _, c := range *calls {
				if strings.Join(c, " ") != strings.Join(want, " ") {
					t.Fatalf("ran %q, want %q", c, want)
				}
			}
		})
	}
}

func TestPodmanDaemonLocal(t *testing.T) {
	cases := []struct {
		name    string
		command string
		env     map[string]string
		out     string
		runErr  error
		want    bool
		wantErr bool
	}{
		{"podman-remote binary", "podman-remote", nil, "", nil, false, false},
		{"CONTAINER_HOST local socket", "podman", map[string]string{"CONTAINER_HOST": "unix:///run/podman/podman.sock"}, "", nil, true, false},
		{"CONTAINER_HOST ssh", "podman", map[string]string{"CONTAINER_HOST": "ssh://core@vm:22/run/podman.sock"}, "", nil, false, false},
		{"no connections: local engine", "podman", nil, "", nil, true, false},
		{"default connection ssh (podman machine)", "podman", nil, "podman-machine-default\tssh://core@127.0.0.1:50000/run/podman/podman.sock\ttrue\n", nil, false, false},
		{"default connection local socket", "podman", nil, "local\tunix:///run/podman/podman.sock\ttrue\nvm\tssh://core@h/x\tfalse\n", nil, true, false},
		{"named connection", "podman", map[string]string{"CONTAINER_CONNECTION": "vm"}, "local\tunix:///run/podman/podman.sock\ttrue\nvm\tssh://core@h/x\tfalse\n", nil, false, false},
		{"named connection missing", "podman", map[string]string{"CONTAINER_CONNECTION": "gone"}, "local\tunix:///run/podman/podman.sock\ttrue\n", nil, false, true},
		{"connections but no default", "podman", nil, "vm\tssh://core@h/x\tfalse\n", nil, false, true},
		{"several defaults", "podman", nil, "a\tunix:///run/a.sock\ttrue\nb\tunix:///run/b.sock\ttrue\n", nil, false, true},
		{"unexpected output", "podman", nil, "garbage line\n", nil, false, true},
		{"list fails", "podman", nil, "", errors.New("exit 125"), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scriptLocality(t, tc.env, tc.out, tc.runErr)
			got, err := podmanDaemonLocal(context.Background(), tc.command)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got (%v, %v), want (%v, err=%v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}
