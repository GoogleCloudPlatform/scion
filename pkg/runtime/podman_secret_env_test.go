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
	stagedsecrets "github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

// Secret env values must not be in "podman run"'s argv, which any local
// user can read from /proc/<pid>/cmdline while podman runs. A mock podman
// records its argv and its environment separately, so the two channels can
// be told apart the way /proc/<pid>/cmdline and /proc/<pid>/environ are.

const (
	podmanSecretSentinel = "FAKE-PODMAN-SECRET-not-a-real-credential"
	podmanAuthSentinel   = "FAKE-PODMAN-AUTH-not-a-real-credential"
)

// writeArgvEnvPodman writes a mock podman that records its argv (one
// argument per line) in argvFile and its environment in envFile, and
// prints mockPodmanID.
func writeArgvEnvPodman(t *testing.T) (bin, argvFile, envFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "mock-podman")
	argvFile = filepath.Join(dir, "argv")
	envFile = filepath.Join(dir, "env")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argvFile + "'\nenv > '" + envFile + "'\necho " + mockPodmanID + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}
	return bin, argvFile, envFile
}

func readRecordedLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func TestPodmanRun_SecretEnvNotInArgv(t *testing.T) {
	bin, argvFile, envFile := writeArgvEnvPodman(t)
	rt := &PodmanRuntime{Command: bin, Rootless: true}
	cfg := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Task:         "hello",
		Env:          []string{"PLAIN_SETTING=visible"},
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "api", Type: "environment", Target: "FAKE_API_KEY", Value: podmanSecretSentinel},
			// A file secret travels in the staged blob, SCION_STAGED_SECRETS.
			{Name: "cred", Type: "file", Target: "~/.cred", Value: podmanSecretSentinel},
		},
		ResolvedAuth: &api.ResolvedAuth{
			Method:  "api-key",
			EnvVars: map[string]string{"FAKE_OAUTH_TOKEN": podmanAuthSentinel},
		},
	}
	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	argv := readRecordedLines(t, argvFile)
	joined := strings.Join(argv, "\n")
	for _, sentinel := range []string{podmanSecretSentinel, podmanAuthSentinel} {
		if strings.Contains(joined, sentinel) {
			t.Errorf("secret %q is in podman's argv:\n%s", sentinel, joined)
		}
	}
	for _, name := range []string{"FAKE_API_KEY", "FAKE_OAUTH_TOKEN", stagedsecrets.EnvVar} {
		if !hasEnvArg(argv, name) {
			t.Errorf("argv lacks a bare -e %s:\n%s", name, joined)
		}
		for _, a := range argv {
			if strings.HasPrefix(a, name+"=") {
				t.Errorf("argv carries %s's value: %q", name, a)
			}
		}
	}
	// Settings that are not secrets stay in argv as they were.
	if !hasEnvArg(argv, "PLAIN_SETTING=visible") {
		t.Errorf("argv lacks -e PLAIN_SETTING=visible:\n%s", joined)
	}

	env := readRecordedLines(t, envFile)
	for _, want := range []string{"FAKE_API_KEY=" + podmanSecretSentinel, "FAKE_OAUTH_TOKEN=" + podmanAuthSentinel} {
		if !slices.Contains(env, want) {
			t.Errorf("podman's environment lacks %q", want)
		}
	}
	if !slices.ContainsFunc(env, func(e string) bool {
		return strings.HasPrefix(e, stagedsecrets.EnvVar+"=") && len(e) > len(stagedsecrets.EnvVar)+1
	}) {
		t.Errorf("podman's environment lacks the staged secrets blob %s", stagedsecrets.EnvVar)
	}
	// podman's own environment is otherwise this process's.
	if p := os.Getenv("PATH"); p != "" && !slices.Contains(env, "PATH="+p) {
		t.Errorf("podman's PATH is not this process's PATH")
	}
}

// Rootless, the pause process podman starts keeps podman's environment for
// its whole life, so it must already run, started without the secret env,
// before "podman run" gets the values. Rootful podman has no pause process.
func TestPodmanRun_PauseProcessStartedWithoutSecretEnv(t *testing.T) {
	for _, rootless := range []bool{true, false} {
		t.Run(fmt.Sprintf("rootless=%v", rootless), func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "mock-podman")
			log := filepath.Join(dir, "calls")
			// One line per call: the subcommand, then "secret" when the
			// call's environment carries the sentinel.
			script := "#!/bin/sh\nif env | grep -q '" + podmanSecretSentinel + "'; then s=secret; else s=clean; fi\n" +
				"echo \"$1 $s\" >> '" + log + "'\nif [ \"$1\" = run ]; then echo " + mockPodmanID + "; fi\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := RunConfig{
				Harness:      &harness.Generic{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				Task:         "hello",
				ResolvedSecrets: []api.ResolvedSecret{
					{Name: "api", Type: "environment", Target: "FAKE_API_KEY", Value: podmanSecretSentinel},
				},
			}
			if _, err := (&PodmanRuntime{Command: bin, Rootless: rootless}).Run(context.Background(), cfg); err != nil {
				t.Fatalf("Run: %v", err)
			}
			want := []string{"unshare clean", "run secret"}
			if !rootless {
				want = []string{"run secret"}
			}
			if got := readRecordedLines(t, log); !slices.Equal(got, want) {
				t.Fatalf("podman calls = %q, want %q", got, want)
			}
		})
	}
}

// hasEnvArg reports whether argv has "-e" followed by value.
func hasEnvArg(argv []string, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-e" && argv[i+1] == value {
			return true
		}
	}
	return false
}

// podmanContainerEnv is the container env podman derives from argv and its
// own environment: "-e"/"--env" before the image, in order, a bare name
// copying podman's value when it has one and otherwise changing nothing.
func podmanContainerEnv(args []string, image string, procEnv []string) map[string]string {
	proc := map[string]string{}
	for _, e := range procEnv {
		k, v, _ := strings.Cut(e, "=")
		proc[k] = v
	}
	got := map[string]string{}
	for i := 0; i+1 < len(args) && args[i] != image; i++ {
		if args[i] != "-e" && args[i] != "--env" {
			continue
		}
		i++
		k, v, set := strings.Cut(args[i], "=")
		if !set {
			pv, ok := proc[k]
			if !ok {
				continue // podman keeps the earlier value
			}
			v = pv
		}
		got[k] = v
	}
	return got
}

// lookupIn is os.LookupEnv over procEnv.
func lookupIn(procEnv []string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		for _, e := range procEnv {
			if k, v, _ := strings.Cut(e, "="); k == name {
				return v, true
			}
		}
		return "", false
	}
}

func TestMoveSecretEnvOutOfArgv(t *testing.T) {
	const image = "img:1"
	tail := []string{image, "sh", "-c", "run -e NOT_A_FLAG=x"}
	procEnv := []string{"HOSTED=from-host", "PATH=/usr/bin"}
	names := map[string]bool{
		"SECRET": true, "HOSTED": true, "TWICE": true, "BARE_LAST": true,
		"PATH": true, "XDG_RUNTIME_DIR": true, "GLOB*": true, "NOT_A_FLAG": true, "EMPTY": true,
		"HTTPS_PROXY": true, "no_proxy": true,
		"DOCKER_CONFIG": true, "SSL_CERT_FILE": true, "_CONTAINERS_USERNS_CONFIGURED": true,
	}
	for _, tc := range []struct {
		name     string
		flags    []string
		wantArgv []string // flags after the rewrite
		wantEnv  []string
	}{
		{
			name:     "value moves to env",
			flags:    []string{"run", "-d", "-e", "SECRET=s1", "-e", "PLAIN=p"},
			wantArgv: []string{"run", "-d", "-e", "SECRET", "-e", "PLAIN=p"},
			wantEnv:  []string{"SECRET=s1"},
		},
		{
			name:     "--env too",
			flags:    []string{"run", "--env", "SECRET=s1"},
			wantArgv: []string{"run", "--env", "SECRET"},
			wantEnv:  []string{"SECRET=s1"},
		},
		{
			name:     "last occurrence wins",
			flags:    []string{"run", "-e", "TWICE=first", "-e", "TWICE", "-e", "TWICE=last"},
			wantArgv: []string{"run", "-e", "TWICE", "-e", "TWICE", "-e", "TWICE"},
			wantEnv:  []string{"TWICE=last"},
		},
		{
			name:     "bare last keeps podman's inherited value",
			flags:    []string{"run", "-e", "HOSTED=secret", "-e", "HOSTED"},
			wantArgv: []string{"run", "-e", "HOSTED", "-e", "HOSTED"},
			wantEnv:  nil,
		},
		{
			name:     "bare and unset keeps the earlier value",
			flags:    []string{"run", "-e", "BARE_LAST=v", "-e", "BARE_LAST"},
			wantArgv: []string{"run", "-e", "BARE_LAST", "-e", "BARE_LAST"},
			wantEnv:  []string{"BARE_LAST=v"},
		},
		{
			name:     "bare and unset alone stays unset",
			flags:    []string{"run", "-e", "BARE_LAST"},
			wantArgv: []string{"run", "-e", "BARE_LAST"},
			wantEnv:  nil,
		},
		{
			name:     "a value after an inherited one wins",
			flags:    []string{"run", "-e", "HOSTED", "-e", "HOSTED=v"},
			wantArgv: []string{"run", "-e", "HOSTED", "-e", "HOSTED"},
			wantEnv:  []string{"HOSTED=v"},
		},
		{
			name:     "names podman reads for registries and TLS stay in argv",
			flags:    []string{"run", "-e", "DOCKER_CONFIG=/c", "-e", "SSL_CERT_FILE=/f", "-e", "_CONTAINERS_USERNS_CONFIGURED=1"},
			wantArgv: []string{"run", "-e", "DOCKER_CONFIG=/c", "-e", "SSL_CERT_FILE=/f", "-e", "_CONTAINERS_USERNS_CONFIGURED=1"},
			wantEnv:  nil,
		},
		{
			name:     "proxy settings podman itself reads stay in argv",
			flags:    []string{"run", "-e", "HTTPS_PROXY=http://user:pw@proxy:3128", "-e", "no_proxy=localhost"},
			wantArgv: []string{"run", "-e", "HTTPS_PROXY=http://user:pw@proxy:3128", "-e", "no_proxy=localhost"},
			wantEnv:  nil,
		},
		{
			name:     "empty value",
			flags:    []string{"run", "-e", "EMPTY="},
			wantArgv: []string{"run", "-e", "EMPTY"},
			wantEnv:  []string{"EMPTY="},
		},
		{
			name:     "names podman reads itself stay in argv",
			flags:    []string{"run", "-e", "PATH=/opt/bin", "-e", "XDG_RUNTIME_DIR=/x"},
			wantArgv: []string{"run", "-e", "PATH=/opt/bin", "-e", "XDG_RUNTIME_DIR=/x"},
			wantEnv:  nil,
		},
		{
			name:     "a pattern name stays in argv",
			flags:    []string{"run", "-e", "GLOB*=v"},
			wantArgv: []string{"run", "-e", "GLOB*=v"},
			wantEnv:  nil,
		},
		{
			name:     "a flag value that is not env is untouched",
			flags:    []string{"run", "--label", "SECRET=label", "-e", "SECRET=s1"},
			wantArgv: []string{"run", "--label", "SECRET=label", "-e", "SECRET"},
			wantEnv:  []string{"SECRET=s1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append([]string(nil), tc.flags...), tail...)
			orig := append([]string(nil), args...)
			got, env, err := moveSecretEnvOutOfArgv(args, image, names, lookupIn(procEnv))
			if err != nil {
				t.Fatalf("moveSecretEnvOutOfArgv: %v", err)
			}
			if !slices.Equal(args, orig) {
				t.Errorf("input argv modified: %q", args)
			}
			if want := append(append([]string(nil), tc.wantArgv...), tail...); !slices.Equal(got, want) {
				t.Errorf("argv = %q, want %q", got, want)
			}
			if !slices.Equal(env, tc.wantEnv) {
				t.Errorf("env = %q, want %q", env, tc.wantEnv)
			}
			// The container env podman derives is unchanged.
			before := podmanContainerEnv(orig, image, procEnv)
			after := podmanContainerEnv(got, image, append(append([]string(nil), procEnv...), env...))
			if len(before) != len(after) {
				t.Errorf("container env = %v, want %v", after, before)
			}
			for k, v := range before {
				if after[k] != v {
					t.Errorf("container env %s = %q, want %q", k, after[k], v)
				}
			}
		})
	}
}

// An argv not shaped as buildCommonRunArgs builds it is refused, not run
// with the values in it.
func TestMoveSecretEnvOutOfArgv_RefusesUnknownShape(t *testing.T) {
	for _, args := range [][]string{
		{"run", "-e", "SECRET=s1", "img:1"},
		{"run", "-e", "SECRET=s1", "other:1", "sh", "-c", "x"},
		{"run", "-e", "SECRET=s1", "img:1", "bash", "-c", "x"},
		{},
	} {
		if _, _, err := moveSecretEnvOutOfArgv(args, "img:1", map[string]bool{"SECRET": true}, lookupIn(nil)); err == nil {
			t.Errorf("moveSecretEnvOutOfArgv(%q) accepted an unknown argv shape", args)
		}
	}
}
