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

package testutil

import (
	"fmt"
	"os"
)

// KubernetesTestNamespace is the namespace PinKubernetesNamespace pins the
// Kubernetes runtime's default-namespace lookup to.
const KubernetesTestNamespace = "default"

// PinKubernetesNamespace makes the Kubernetes runtime's default-namespace
// lookup resolve to KubernetesTestNamespace for the whole test binary, no
// matter where the tests run (ptone/scion#4999).
//
// With no namespace configured, the runtime reads SCION_K8S_NAMESPACE, then
// POD_NAMESPACE, then the in-cluster service-account namespace file, and
// only then falls back to "default". Inside a Kubernetes pod the file (and
// often POD_NAMESPACE) name the pod's own namespace, so tests that expect
// "default" would fail there. Setting SCION_K8S_NAMESPACE, which ranks first,
// overrides the file; unsetting POD_NAMESPACE drops a leaked value too. The
// pin is process-wide, set once from TestMain, because the lookup happens in
// many tests and helpers and a leaked environment affects all of them alike.
//
// Tests that exercise a specific namespace still override it with
// t.Setenv("SCION_K8S_NAMESPACE", ...); Go restores the pinned value once such
// a test finishes. Unsetting it or setting it to "" falls through to the host
// sources again. The helpers that scrub every SCION_* variable for a test
// (clearSCIONEnv in pkg/runtimebroker, isolateTestEnv in pkg/agent, and
// similar inline loops) do exactly that, so they lift the pin for the length
// of their test. A test that scrubs SCION_* and also resolves the default
// namespace must set SCION_K8S_NAMESPACE itself.
//
// Call it once from TestMain, before m.Run(). Any failure exits the process.
func PinKubernetesNamespace() {
	if err := os.Setenv("SCION_K8S_NAMESPACE", KubernetesTestNamespace); err != nil {
		fmt.Fprintf(os.Stderr, "testutil.PinKubernetesNamespace: setting SCION_K8S_NAMESPACE: %v\n", err)
		os.Exit(1)
	}
	if err := os.Unsetenv("POD_NAMESPACE"); err != nil {
		fmt.Fprintf(os.Stderr, "testutil.PinKubernetesNamespace: unsetting POD_NAMESPACE: %v\n", err)
		os.Exit(1)
	}
}
