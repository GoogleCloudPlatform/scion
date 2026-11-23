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

package brokercontrol

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func setSocketTestHooks(t *testing.T, home string, env map[string]string, uid, euid int, static []string) {
	t.Helper()
	prevHome := userHomeDirFunc
	prevEnv := getEnvFunc
	prevUID := getUIDFunc
	prevEUID := getEUIDFunc
	prevStatic := unixStaticSocketPaths

	userHomeDirFunc = func() (string, error) { return home, nil }
	getEnvFunc = func(key string) string { return env[key] }
	getUIDFunc = func() int { return uid }
	getEUIDFunc = func() int { return euid }
	if static != nil {
		unixStaticSocketPaths = static
	}

	t.Cleanup(func() {
		userHomeDirFunc = prevHome
		getEnvFunc = prevEnv
		getUIDFunc = prevUID
		getEUIDFunc = prevEUID
		unixStaticSocketPaths = prevStatic
	})
}

func wantSocketRefused(t *testing.T, err error, rule Rule) {
	t.Helper()
	wantRefused(t, err, rule)
	var re *RefusalError
	if !errors.As(err, &re) {
		t.Fatalf("error %T is not a *RefusalError", err)
	}
	if rule != RuleUnresolved && re.Class != ClassContainerEngineSocket {
		t.Fatalf("class = %q, want %q (%s)", re.Class, ClassContainerEngineSocket, re.Detail())
	}
}

func TestCheck_HomeDockerSocketAndParent(t *testing.T) {
	f := newFixture(t)
	setSocketTestHooks(t, f.home, nil, 1000, 1000, nil)

	dockerDir := filepath.Join(f.home, ".docker")
	dockerSock := filepath.Join(dockerDir, "run", "docker.sock")
	dockerConfig := filepath.Join(dockerDir, "config.json")
	write(t, dockerSock)
	write(t, dockerConfig)

	// Direct socket mount refused.
	_, err := Check(dockerSock, VolumeAllowed)
	wantSocketRefused(t, err, RuleEqual)

	// Parent directory ~/.docker refused as an ancestor of ~/.docker/run/docker.sock.
	_, err = Check(dockerDir, VolumeAllowed)
	wantSocketRefused(t, err, RuleAncestor)

	// ~/.docker/run refused as an ancestor.
	_, err = Check(filepath.Join(dockerDir, "run"), VolumeAllowed)
	wantSocketRefused(t, err, RuleAncestor)

	// Sibling ~/.docker/config.json remains allowed.
	if _, err := Check(dockerConfig, VolumeAllowed); err != nil {
		t.Fatalf("mounting ~/.docker/config.json should be allowed, got: %v", err)
	}
}

func TestCheck_DockerAndContainerHostEnv(t *testing.T) {
	f := newFixture(t)
	customDocker := filepath.Join(f.outside, "custom-docker", "docker.sock")
	customPodman := filepath.Join(f.outside, "custom-podman", "podman.sock")
	nonExistent := filepath.Join(f.outside, "missing-dir", "engine.sock")
	write(t, customDocker)
	write(t, customPodman)

	toUnixURI := func(p string) string {
		// On Unix, filepath.ToSlash(p) starts with "/".
		return "unix://" + filepath.ToSlash(p)
	}

	if runtime.GOOS == "windows" {
		// On Windows, unix:///C:/... does not have filepath.IsAbs(u.Path) == true
		// because url.Parse yields "/C:/...". Skip URI path construction on Windows.
		t.Skip("unix:// socket URIs use POSIX absolute paths")
	}

	t.Run("DOCKER_HOST and CONTAINER_HOST unix sockets refused", func(t *testing.T) {
		setSocketTestHooks(t, f.home, map[string]string{
			"DOCKER_HOST":    toUnixURI(customDocker),
			"CONTAINER_HOST": toUnixURI(customPodman),
		}, 1000, 1000, nil)

		_, err := Check(customDocker, VolumeAllowed)
		wantSocketRefused(t, err, RuleEqual)

		_, err = Check(filepath.Dir(customDocker), VolumeAllowed)
		wantSocketRefused(t, err, RuleAncestor)

		_, err = Check(customPodman, VolumeAllowed)
		wantSocketRefused(t, err, RuleEqual)

		_, err = Check(filepath.Dir(customPodman), VolumeAllowed)
		wantSocketRefused(t, err, RuleAncestor)

		// Symlink to DOCKER_HOST socket is also refused.
		link := filepath.Join(f.outside, "sneaky.sock")
		symlink(t, customDocker, link)
		_, err = Check(link, VolumeAllowed)
		wantSocketRefused(t, err, RuleEqual)
		var re *RefusalError
		if !errors.As(err, &re) {
			t.Fatalf("error %T is not a *RefusalError", err)
		}
		// Error message must not leak the resolved host socket path or mention ~/.scion.
		msg := err.Error()
		if strings.Contains(msg, customDocker) {
			t.Fatalf("error leaked resolved socket path: %s", msg)
		}
		if strings.Contains(msg, ".scion") {
			t.Fatalf("container-engine socket error has misleading ~/.scion wording: %s", msg)
		}
		if !strings.Contains(msg, link) {
			t.Fatalf("error should name the configured source %q: %s", link, msg)
		}
		if !strings.Contains(re.Detail(), customDocker) {
			t.Fatalf("operator detail should include resolved path: %s", re.Detail())
		}
	})

	t.Run("non-existent DOCKER_HOST unix socket still refused", func(t *testing.T) {
		setSocketTestHooks(t, f.home, map[string]string{
			"DOCKER_HOST": toUnixURI(nonExistent),
		}, 1000, 1000, nil)

		_, err := Check(nonExistent, VolumeAllowed)
		wantSocketRefused(t, err, RuleEqual)
	})

	t.Run("non-unix schemes ignored and do not break allowed mounts", func(t *testing.T) {
		setSocketTestHooks(t, f.home, map[string]string{
			"DOCKER_HOST":    "tcp://127.0.0.1:2375",
			"CONTAINER_HOST": "ssh://user@example.com/run/podman/podman.sock",
		}, 1000, 1000, nil)

		allowed := filepath.Join(f.outside, "work")
		mkdir(t, allowed)
		if _, err := Check(allowed, VolumeAllowed); err != nil {
			t.Fatalf("allowed mount failed with non-unix DOCKER_HOST/CONTAINER_HOST: %v", err)
		}
	})
}

func TestParseUnixSocketEndpoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX absolute unix:// paths")
	}
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"unix:///var/run/docker.sock", "/var/run/docker.sock", true},
		{"  unix:///run/podman/podman.sock  ", "/run/podman/podman.sock", true},
		{"UNIX:///run/docker.sock", "/run/docker.sock", true},
		{"tcp://127.0.0.1:2375", "", false},
		{"http://localhost:2375", "", false},
		{"https://docker.example.com:2376", "", false},
		{"ssh://user@host/run/podman/podman.sock", "", false},
		{"npipe:////./pipe/docker_engine", "", false},
		{"unix://relative.sock", "", false},
		{"unix://localhost/var/run/docker.sock", "", false},
		{"/var/run/docker.sock", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := parseUnixSocketEndpoint(tc.raw)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseUnixSocketEndpoint(%q) = (%q, %v), want (%q, %v)", tc.raw, got, ok, tc.want, tc.ok)
		}
	}
}
