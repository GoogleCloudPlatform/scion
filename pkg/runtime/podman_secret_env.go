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
	"fmt"
	"regexp"
	"strings"

	stagedsecrets "github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

// Secret env values stay out of "podman run"'s argv. Any local user can
// read a process's argv from /proc/<pid>/cmdline for as long as it runs,
// and "podman run" runs for the whole create, minutes when an image is
// pulled first. Given a bare "-e KEY", podman copies KEY's value from its
// own environment, which only the same user can read
// (/proc/<pid>/environ). So for each secret name the argv carries "-e KEY"
// and the value goes in the podman process's environment.

// podmanSecretEnvNames returns the env names whose values are credentials
// on a container launch: the staged secrets blob, every environment-type
// resolved secret and every resolved-auth env entry. config must already
// have been through prepareContainerSecretEnv.
func podmanSecretEnvNames(config RunConfig) map[string]bool {
	names := map[string]bool{stagedsecrets.EnvVar: true}
	for _, s := range config.ResolvedSecrets {
		if s.Type == "environment" || s.Type == "" {
			names[s.Target] = true
		}
	}
	if config.ResolvedAuth != nil {
		for k := range config.ResolvedAuth.EnvVars {
			names[k] = true
		}
	}
	return names
}

// podmanEnvNamePattern is an env name podman takes literally after a bare
// "-e": a "*" would make it a pattern over podman's environment.
var podmanEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// podmanOwnEnvName reports whether podman itself reads name, so setting it
// in podman's environment would change how podman runs, not only what the
// container gets. A secret with such a name keeps its value in argv, as
// before.
func podmanOwnEnvName(name string) bool {
	switch name {
	case "PATH", "HOME", "USER", "TMPDIR", "SHELL", "LANG", "TZ",
		"CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINER_SSHKEY", "CONTAINER_PASSPHRASE",
		"REGISTRY_AUTH_FILE", "STORAGE_DRIVER", "STORAGE_OPTS", "NOTIFY_SOCKET",
		"DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK",
		"GODEBUG", "GOGC", "GOMAXPROCS", "GOMEMLIMIT", "GOTRACEBACK",
		// podman's own registry and network calls read these
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy":
		return true
	}
	for _, prefix := range []string{"XDG_", "CONTAINERS_", "PODMAN_", "BUILDAH_", "LD_", "LC_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// moveSecretEnvOutOfArgv rewrites every "-e NAME=VALUE" (or "--env") for a
// name in names into a bare "-e NAME" and returns the rewritten argv with
// the environment entries ("NAME=VALUE") podman must be given. Only the
// flags before image are rewritten: the container command after it is not
// podman's to parse. args must end with image and "sh", "-c", <command>,
// as buildCommonRunArgs builds them; any other shape is refused rather
// than run with the values in argv.
//
// What the container receives is unchanged. Podman keeps the last "-e"
// for a name, so the value of the last occurrence is the one given; when
// that occurrence is already bare, podman's inherited value is the one
// used, as before, and nothing is added.
func moveSecretEnvOutOfArgv(args []string, image string, names map[string]bool) ([]string, []string, error) {
	end := len(args) - 4
	if end < 0 || args[end] != image || args[end+1] != "sh" || args[end+2] != "-c" {
		return nil, nil, fmt.Errorf("podman run arguments do not end with the image and its command; refusing to start with secret env in argv")
	}
	out := append([]string(nil), args...)
	type final struct {
		value string
		set   bool
	}
	last := map[string]final{}
	var order []string
	for i := 0; i < end-1; i++ {
		if out[i] != "-e" && out[i] != "--env" {
			continue
		}
		i++
		name, value, set := strings.Cut(out[i], "=")
		if !names[name] || !podmanEnvNamePattern.MatchString(name) || podmanOwnEnvName(name) {
			continue
		}
		out[i] = name
		if _, seen := last[name]; !seen {
			order = append(order, name)
		}
		last[name] = final{value: value, set: set}
	}
	var env []string
	for _, name := range order {
		if f := last[name]; f.set {
			env = append(env, name+"="+f.value)
		}
	}
	return out, env, nil
}
