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
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

// kubeconfigWithServer is a kubeconfig whose one context selects a cluster
// with the given server and a user with no credentials.
func kubeconfigWithServer(server string) string {
	return `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: ` + server + `
  name: c
contexts:
- context:
    cluster: c
    user: u
  name: x
current-context: x
users:
- name: u
  user: {}
`
}

// A kubeconfig that resolves to client-go's default configuration (only
// the default server, as KUBERNETES_MASTER sets it, and no credentials) is
// one the deferred loader may replace with in-cluster credentials, so its
// connection mode is Unknown, not Kubeconfig. The same file with another
// server stays Kubeconfig.
func TestLoadClientConfig_DefaultConfigIsUnknown(t *testing.T) {
	// No in-cluster configuration is possible here, so the loader returns
	// the default configuration itself rather than in-cluster credentials.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	path := writeTestKubeconfig(t, kubeconfigWithServer(clientcmd.ClusterDefaults.Server))
	_, src, err := loadClientConfig(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if src.connection != ConnUnknown {
		t.Errorf("default-server kubeconfig: connection = %v, want %v", src.connection, ConnUnknown)
	}
	if src.currentContext != "x" {
		t.Errorf("currentContext = %q, want x", src.currentContext)
	}

	path = writeTestKubeconfig(t, kubeconfigWithServer("https://127.0.0.1:6443"))
	_, src, err = loadClientConfig(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if src.connection != ConnKubeconfig {
		t.Errorf("explicit-server kubeconfig: connection = %v, want %v", src.connection, ConnKubeconfig)
	}
}
