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

// ScopeIdentity returns the normative identity string of a scope:
// "docker:<daemon ID>", "kubernetes:<cluster UID>/<namespace>" or
// "cloudrun:<project number>/<region>". Endpoints, API server URLs, context
// aliases, project IDs, service accounts, networks and credentials are never
// part of it. A scope without its identity fields is
// ErrExecutionScopeUnidentified. It is the one source of the scope identity:
// VerifyScope compares it, and the host groups instances by it
// (brokerhost.ScopeKey).
func ScopeIdentity(s ExecutionScope) (string, error) {
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
	case TargetTypeCloudRun:
		if s.CloudRun == nil || strings.TrimSpace(s.CloudRun.ProjectNumber) == "" || strings.TrimSpace(s.CloudRun.Region) == "" {
			return "", fmt.Errorf("%w: cloudrun scope needs a project number and a region", ErrExecutionScopeUnidentified)
		}
		return "cloudrun:" + s.CloudRun.ProjectNumber + "/" + s.CloudRun.Region, nil
	default:
		return "", fmt.Errorf("%w: unknown scope type %q", ErrExecutionScopeUnidentified, s.Type)
	}
}

// VerifyScope compares a recorded execution scope with the one observed at
// start. Only identity fields count: a changed Docker endpoint, Kubernetes
// API server URL or Cloud Run project ID with the same daemon ID, cluster
// UID and namespace, or project number and region, is the same target (the
// recorded project ID is kept; LoadOrCreate logs the accepted alias without
// its values). A different identity, or an observed scope without one, is an
// error; existing placement is never re-pointed.
func VerifyScope(recorded, observed ExecutionScope) error {
	want, err := ScopeIdentity(recorded)
	if err != nil {
		return fmt.Errorf("recorded scope: %w", err)
	}
	if observed.Type != recorded.Type {
		return fmt.Errorf("%w: recorded type %q, observed %q", ErrExecutionScopeChanged, recorded.Type, observed.Type)
	}
	got, err := ScopeIdentity(observed)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: recorded %s, observed %s; restore the original runtime scope or configure a new instance key",
			ErrExecutionScopeChanged, want, got)
	}
	return nil
}
