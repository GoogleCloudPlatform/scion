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
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Injectable host-environment hooks so tests can exercise rootful, rootless,
// and env-configured container-engine socket paths without mutating process
// globals or depending on host filesystem permissions.
var (
	userHomeDirFunc = os.UserHomeDir
	getEnvFunc      = os.Getenv
	getUIDFunc      = os.Getuid
	getEUIDFunc     = os.Geteuid

	// unixStaticSocketPaths are the well-known rootful Docker and Podman
	// socket locations on Unix hosts.
	unixStaticSocketPaths = []string{
		"/var/run/docker.sock",
		"/run/docker.sock",
		"/run/podman/podman.sock",
		"/var/run/podman/podman.sock",
	}
)

// containerEngineSocketPaths returns the candidate host paths of well-known
// rootful/rootless Docker and Podman sockets plus any unix:// socket configured
// via DOCKER_HOST or CONTAINER_HOST.
func containerEngineSocketPaths() []string {
	var paths []string
	if runtime.GOOS != "windows" {
		paths = append(paths, unixStaticSocketPaths...)
		if xdg := getEnvFunc("XDG_RUNTIME_DIR"); xdg != "" && filepath.IsAbs(xdg) {
			paths = append(paths,
				filepath.Join(xdg, "docker.sock"),
				filepath.Join(xdg, "podman", "podman.sock"),
			)
		}
		uid := getUIDFunc()
		if uid >= 0 {
			uidDir := filepath.Join("/run/user", strconv.Itoa(uid))
			paths = append(paths,
				filepath.Join(uidDir, "docker.sock"),
				filepath.Join(uidDir, "podman", "podman.sock"),
			)
		}
		if euid := getEUIDFunc(); euid >= 0 && euid != uid {
			euidDir := filepath.Join("/run/user", strconv.Itoa(euid))
			paths = append(paths,
				filepath.Join(euidDir, "docker.sock"),
				filepath.Join(euidDir, "podman", "podman.sock"),
			)
		}
	}
	if home, err := userHomeDirFunc(); err == nil && home != "" && filepath.IsAbs(home) {
		paths = append(paths, filepath.Join(home, ".docker", "run", "docker.sock"))
	}
	for _, envName := range []string{"DOCKER_HOST", "CONTAINER_HOST"} {
		if sock, ok := parseUnixSocketEndpoint(getEnvFunc(envName)); ok {
			paths = append(paths, sock)
		}
	}
	return paths
}

// parseUnixSocketEndpoint extracts the absolute filesystem path from a
// unix:// container-engine endpoint URI (as used in DOCKER_HOST and
// CONTAINER_HOST). Non-unix schemes (tcp://, http://, ssh://, npipe://, etc.),
// empty values, and relative paths return ("", false).
func parseUnixSocketEndpoint(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", false
	}
	if u.Scheme != "unix" || u.Host != "" || u.Path == "" || !filepath.IsAbs(u.Path) {
		return "", false
	}
	return u.Path, true
}

// resolveSocketPath resolves a container-engine socket path for the protected
// set without failing Resolve() globally when an intermediate directory is
// unreadable (for example 0700 root:root /run/podman on a non-root broker) or
// a well-known socket path is a dangling symlink. It resolves the deepest
// accessible existing ancestor through symlinks and appends the remaining
// components, falling back to filepath.Clean(p) if no ancestor can be
// resolved. Returns ("", false) only if p is empty or not absolute.
func resolveSocketPath(p string) (string, bool) {
	if p == "" || !filepath.IsAbs(p) {
		return "", false
	}
	if r, err := resolvePhysical(p); err == nil {
		return r, true
	}
	cleaned := filepath.Clean(p)
	vol := filepath.VolumeName(cleaned)
	parts := strings.Split(cleaned[len(vol):], sep)
	for i := len(parts) - 1; i > 0; i-- {
		prefix := vol + strings.Join(parts[:i], sep)
		if prefix == vol {
			prefix = vol + sep
		}
		if resolved, err := filepath.EvalSymlinks(prefix); err == nil {
			missing := parts[i:]
			return filepath.Clean(filepath.Join(append([]string{resolved}, missing...)...)), true
		}
	}
	return cleaned, true
}
