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

package agent

import (
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
)

// TestMain pins the Kubernetes runtime's default namespace to "default", so
// tests that build the Kubernetes runtime without a namespace give the same
// result inside and outside a Kubernetes pod (ptone/scion#4999).
func TestMain(m *testing.M) {
	testutil.PinKubernetesNamespace()
	os.Exit(m.Run())
}

// TestKubernetesDefaultNamespace_IgnoresHostNamespace guards the TestMain
// pin. POD_NAMESPACE stands in for a host whose namespace is not "default":
// it ranks above the in-cluster service-account namespace file, so if the
// pin outranks it, it outranks the file as well.
func TestKubernetesDefaultNamespace_IgnoresHostNamespace(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "host-ns")

	if got := runtime.DefaultKubernetesNamespace(); got != testutil.KubernetesTestNamespace {
		t.Errorf("DefaultKubernetesNamespace() = %q, want %q", got, testutil.KubernetesTestNamespace)
	}
	if got := runtime.NewKubernetesRuntime(nil).DefaultNamespace; got != testutil.KubernetesTestNamespace {
		t.Errorf("NewKubernetesRuntime(nil).DefaultNamespace = %q, want %q", got, testutil.KubernetesTestNamespace)
	}
}
