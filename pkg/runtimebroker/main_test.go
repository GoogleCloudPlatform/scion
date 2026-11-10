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

package runtimebroker

import (
	"os"
	"testing"

	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
)

// TestMain isolates $HOME for the whole pkg/runtimebroker test binary.
// The broker resolves its global dir (~/.scion/projects,
// ~/.scion/project-configs, ~/.scion/runtime-broker-state, caches) through
// HOME, so without this, tests write into the real developer/agent HOME
// (ptone/scion#2445).
//
// It also pins the Kubernetes runtime's default namespace to "default", so
// tests that build the Kubernetes runtime or resolve a namespace from it
// give the same result inside and outside a Kubernetes pod
// (ptone/scion#4999).
func TestMain(m *testing.M) {
	teardown := testutil.IsolateHome("scion-runtimebroker-test-home-*")
	testutil.PinKubernetesNamespace()
	code := m.Run()
	teardown()
	os.Exit(code)
}

// TestKubernetesDefaultNamespace_IgnoresHostNamespace guards the TestMain
// pin. POD_NAMESPACE stands in for a host whose namespace is not "default":
// it ranks above the in-cluster service-account namespace file, so if the
// pin outranks it, it outranks the file as well.
func TestKubernetesDefaultNamespace_IgnoresHostNamespace(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "host-ns")

	if got := scionrt.DefaultKubernetesNamespace(); got != testutil.KubernetesTestNamespace {
		t.Errorf("DefaultKubernetesNamespace() = %q, want %q", got, testutil.KubernetesTestNamespace)
	}
	if got := scionrt.NewKubernetesRuntime(nil).DefaultNamespace; got != testutil.KubernetesTestNamespace {
		t.Errorf("NewKubernetesRuntime(nil).DefaultNamespace = %q, want %q", got, testutil.KubernetesTestNamespace)
	}
}
