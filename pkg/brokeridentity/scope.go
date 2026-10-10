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

package brokeridentity

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// NormalizeDockerEndpoint normalizes a Docker daemon endpoint as reported by
// the docker CLI: the scheme is lower-cased, unix socket paths are cleaned and
// tcp endpoints get an explicit port. The result is informational only.
func NormalizeDockerEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" {
		return endpoint
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "unix", "npipe":
		p := u.Path
		if p == "" {
			p = u.Opaque
		}
		return scheme + "://" + path.Clean(p)
	case "tcp", "http", "https":
		host := strings.ToLower(u.Hostname())
		port := u.Port()
		if port == "" {
			port = "2375"
			if scheme == "https" {
				port = "2376"
			}
		}
		return scheme + "://" + host + ":" + port
	default:
		return scheme + "://" + u.Host + u.Path
	}
}

// NormalizeKubernetesAPIServer normalizes a Kubernetes API server URL
// (informational only): lower-cased scheme and host, explicit port, no
// trailing slash.
func NormalizeKubernetesAPIServer(server string) string {
	u, err := url.Parse(strings.TrimSpace(server))
	if err != nil || u.Scheme == "" {
		return strings.TrimSpace(server)
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if strings.EqualFold(u.Scheme, "http") {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Hostname()) + ":" + port + strings.TrimRight(u.Path, "/")
}

// scopeIdentity returns the identity string of a scope: the Docker daemon ID,
// or the Kubernetes cluster UID plus namespace. Endpoints, API server URLs,
// context aliases and credentials are never part of it.
func scopeIdentity(s ExecutionScope) (string, error) {
	switch s.Type {
	case TargetTypeDocker:
		if s.Docker == nil || strings.TrimSpace(s.Docker.DaemonID) == "" {
			return "", fmt.Errorf("%w: docker scope has no daemon ID", ErrExecutionScopeUnidentified)
		}
		return "docker:" + s.Docker.DaemonID, nil
	case TargetTypeKubernetes:
		if s.Kubernetes == nil || strings.TrimSpace(s.Kubernetes.ClusterUID) == "" || strings.TrimSpace(s.Kubernetes.Namespace) == "" {
			return "", fmt.Errorf("%w: kubernetes scope needs a cluster UID and a namespace", ErrExecutionScopeUnidentified)
		}
		return "kubernetes:" + s.Kubernetes.ClusterUID + "/" + s.Kubernetes.Namespace, nil
	default:
		return "", fmt.Errorf("%w: unknown scope type %q", ErrExecutionScopeUnidentified, s.Type)
	}
}

// VerifyScope compares a recorded execution scope with the one observed at
// start. Only identity fields count: a changed Docker endpoint or Kubernetes
// API server URL with the same daemon ID or cluster UID and namespace is the
// same target. A different identity, or an observed scope without one, is an
// error; existing placement is never re-pointed.
func VerifyScope(recorded, observed ExecutionScope) error {
	want, err := scopeIdentity(recorded)
	if err != nil {
		return fmt.Errorf("recorded scope: %w", err)
	}
	if observed.Type != recorded.Type {
		return fmt.Errorf("%w: recorded type %q, observed %q", ErrExecutionScopeChanged, recorded.Type, observed.Type)
	}
	got, err := scopeIdentity(observed)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: recorded %s, observed %s; restore the original runtime scope or configure a new instance key",
			ErrExecutionScopeChanged, want, got)
	}
	return nil
}
