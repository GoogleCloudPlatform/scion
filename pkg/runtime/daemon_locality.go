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
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// daemonLocalityEnv and daemonLocalityCommand are the environment and the
// command runner the locality checks consult; variables so tests can
// script every branch.
var (
	daemonLocalityEnv     = os.Getenv
	daemonLocalityCommand = runStdoutCommand
)

// errDaemonLocalityUnknown is returned when the container daemon's endpoint
// cannot be determined; callers treat the daemon as not local.
var errDaemonLocalityUnknown = errors.New("the container daemon's endpoint could not be determined")

// endpointIsLocalSocket reports whether a daemon endpoint URI is a local
// unix socket (unix:///path). Every other form (tcp, ssh, http, https,
// npipe, a bare path, an empty or unparsable value) is not.
func endpointIsLocalSocket(endpoint string) bool {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return false
	}
	return u.Scheme == "unix" && u.Host == "" && u.Path != "" && filepath.IsAbs(u.Path)
}

// dockerDaemonLocal reports whether the Docker CLI command talks to a daemon
// over a local unix socket. The endpoint is the CLI's own: DOCKER_HOST when
// set (it takes precedence over contexts), otherwise the host of the
// context the CLI resolves as current ("docker context inspect" with no
// name honours DOCKER_CONTEXT and the CLI configuration's currentContext).
// Any failure is an error, never a guess.
func dockerDaemonLocal(ctx context.Context, command string) (bool, error) {
	if h := daemonLocalityEnv("DOCKER_HOST"); h != "" {
		return endpointIsLocalSocket(h), nil
	}
	out, err := daemonLocalityCommand(ctx, command, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	if err != nil {
		return false, fmt.Errorf("%w: docker context inspect: %v", errDaemonLocalityUnknown, err)
	}
	host := strings.TrimSpace(out)
	if host == "" || strings.Contains(host, "\n") {
		return false, fmt.Errorf("%w: docker context inspect returned %q", errDaemonLocalityUnknown, host)
	}
	return endpointIsLocalSocket(host), nil
}

// podmanDaemonLocal reports whether the Podman CLI command uses a local
// engine. A podman-remote binary is always remote. CONTAINER_HOST, when set,
// is the endpoint. Otherwise the system connection named by
// CONTAINER_CONNECTION, or else the default connection, is used. With no
// connection at all (and none named) Podman runs its local engine directly.
// Any failure, an unknown named connection or an ambiguous default is an
// error.
func podmanDaemonLocal(ctx context.Context, command string) (bool, error) {
	if strings.Contains(filepath.Base(command), "podman-remote") {
		return false, nil
	}
	if h := daemonLocalityEnv("CONTAINER_HOST"); h != "" {
		return endpointIsLocalSocket(h), nil
	}
	out, err := daemonLocalityCommand(ctx, command, "system", "connection", "list", "--format", "{{.Name}}\t{{.URI}}\t{{.Default}}")
	if err != nil {
		return false, fmt.Errorf("%w: podman system connection list: %v", errDaemonLocalityUnknown, err)
	}
	type conn struct{ name, uri string }
	var all, defaults []conn
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			return false, fmt.Errorf("%w: unexpected podman connection line %q", errDaemonLocalityUnknown, line)
		}
		c := conn{name: strings.TrimSpace(f[0]), uri: strings.TrimSpace(f[1])}
		all = append(all, c)
		if strings.TrimSpace(f[2]) == "true" {
			defaults = append(defaults, c)
		}
	}
	if name := daemonLocalityEnv("CONTAINER_CONNECTION"); name != "" {
		for _, c := range all {
			if c.name == name {
				return endpointIsLocalSocket(c.uri), nil
			}
		}
		return false, fmt.Errorf("%w: podman connection %q not found", errDaemonLocalityUnknown, name)
	}
	switch {
	case len(all) == 0:
		return true, nil
	case len(defaults) == 1:
		return endpointIsLocalSocket(defaults[0].uri), nil
	default:
		return false, fmt.Errorf("%w: %d podman connections and %d defaults", errDaemonLocalityUnknown, len(all), len(defaults))
	}
}
