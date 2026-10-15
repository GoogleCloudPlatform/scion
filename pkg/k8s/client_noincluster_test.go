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

package k8s

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// NewClientWithContextNoInCluster (ptone/scion#5088). These tests use
// t.Setenv, so none of them is parallel. The in-cluster token-file leg of
// client-go's deferred loader needs the fixed service-account path of a pod
// and cannot be exercised here; the constructor's direct configuration has
// no in-cluster leg by construction.

const noInClusterKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
- context:
    cluster: test-cluster
    user: test-user
  name: other-context
current-context: test-context
users:
- name: test-user
  user:
    token: fake-token
`

// With the in-cluster service variables set and no kubeconfig, there is no
// client, and the error is the disabled in-cluster leg.
func TestNewClientWithContextNoInCluster_InClusterEnvNoKubeconfig(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.43.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing.kubeconfig"))
	t.Setenv("HOME", t.TempDir())

	client, err := NewClientWithContextNoInCluster("", "")
	if client != nil || !errors.Is(err, errInClusterDisabled) {
		t.Fatalf("client = %v, err = %v; want no client and errInClusterDisabled", client, err)
	}
}

// A kubeconfig from KUBECONFIG loads its current context, or the context
// asked for.
func TestNewClientWithContextNoInCluster_LoadsKubeconfig(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.43.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	t.Setenv("KUBECONFIG", writeTestKubeconfig(t, noInClusterKubeconfig))

	client, err := NewClientWithContextNoInCluster("", "")
	if err != nil {
		t.Fatalf("current context: %v", err)
	}
	if client.CurrentContext != "test-context" || client.Config.Host != "https://127.0.0.1:6443" {
		t.Fatalf("current context: got %q, host %q", client.CurrentContext, client.Config.Host)
	}
	client, err = NewClientWithContextNoInCluster("", "other-context")
	if err != nil || client.CurrentContext != "other-context" {
		t.Fatalf("explicit context: client %v, err %v; want other-context", client, err)
	}
}

// A context the kubeconfig does not define is an error, not a fallback.
func TestNewClientWithContextNoInCluster_UnknownContextIsAnError(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.43.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	t.Setenv("KUBECONFIG", writeTestKubeconfig(t, noInClusterKubeconfig))

	client, err := NewClientWithContextNoInCluster("", "no-such-context")
	if client != nil || err == nil {
		t.Fatalf("client = %v, err = %v; want an error", client, err)
	}
	if errors.Is(err, errInClusterDisabled) {
		t.Fatalf("an explicit context must not reach the in-cluster leg: %v", err)
	}
}

// A KUBECONFIG path list keeps working: an absent entry is skipped.
func TestNewClientWithContextNoInCluster_KubeconfigPathList(t *testing.T) {
	realPath := writeTestKubeconfig(t, noInClusterKubeconfig)
	list := filepath.Join(t.TempDir(), "absent.kubeconfig") + string(os.PathListSeparator) + realPath
	t.Setenv("KUBECONFIG", list)

	client, err := NewClientWithContextNoInCluster("", "")
	if err != nil || client.CurrentContext != "test-context" {
		t.Fatalf("path list: client %v, err %v; want test-context", client, err)
	}
}
