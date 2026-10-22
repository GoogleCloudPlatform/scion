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

package cmd

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/internal/writesonread"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
)

// TestMain disables the UTC pin for the whole cmd test binary.
//
// Production run functions pin time.Local to UTC; in the shared test binary
// that would race leaked goroutines (other tests' background work reads
// time.Local concurrently) and would silently turn every later TZ=... test
// in this binary into a UTC test, masking real timezone bugs. pkg/util's
// TestPinProcessUTC keeps covering PinProcessUTC itself, and
// pin_process_utc_test.go's AST test covers where the seam is called from.
func TestMain(m *testing.M) {
	pinProcessUTC = func() {}
	clearAmbientScionEnv()
	// The CLI imports the global settings' hub.endpoint (and a linked
	// project's hub.endpoint) into ~/.scion/context.yaml the first time it
	// reads the context, and that import writes the file. Most cmd tests
	// run the root command without an isolated HOME, so with the import on
	// they would write the real user's context.yaml, and tests that write
	// a hub.endpoint would change behaviour. Tests of the imports turn them
	// back on with a temp HOME (enableGlobalHubImport). This is the only
	// place they are turned off.
	importGlobalHubSettings = false
	// A flat Kubernetes instance with no kubeconfig builds its client
	// from the default loading rules only: no test uses the in-cluster
	// config of the pod it runs in (ptone/scion#5088). This is the only
	// place it is set.
	productionFlatDefaultKubernetesClient = flatDefaultKubernetesClient
	flatDefaultKubernetesClient = k8s.NewClientWithContextNoInCluster
	teardown := integrationTestSetup()
	code := m.Run()
	teardown()
	// The no-write switch (internal/writesonread) is process-wide: a test
	// that sets it without runPreflightForTest, whose isolation turns it
	// off again, would leave later tests running without the migrations.
	if writesonread.Disabled() {
		fmt.Fprintln(os.Stderr, "a cmd test left the no-write switch on; set it only through runPreflightForTest")
		code = 1
	}
	os.Exit(code)
}

// productionFlatDefaultKubernetesClient is flatDefaultKubernetesClient's
// value before TestMain replaces it, so a test can pin the production value.
var productionFlatDefaultKubernetesClient func(kubeconfigPath, contextName string) (*k8s.Client, error)

// integrationTestSetup runs before the cmd tests and returns their teardown.
// It does nothing by default; main_integration_test.go replaces it under
// -tags integration to provision the enttest PostgreSQL database.
var integrationTestSetup = func() (teardown func()) { return func() {} }

// clearAmbientScionEnv unsets every SCION_* variable inherited from the
// environment before any cmd test runs. Inside an agent container these
// point at the live hub (SCION_HUB_ENDPOINT, SCION_AUTH_TOKEN, ...) and take
// precedence over the mock servers and temp projects the tests set up, so
// tests would call the real hub and fail with 401s or wrong request counts
// (ptone/scion#2102). Tests that need a value set it themselves with
// t.Setenv. SCION_TEST_* variables are deliberate test opt-ins (for example
// SCION_TEST_BASH) and are kept.
func clearAmbientScionEnv() {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "SCION_") && !strings.HasPrefix(name, "SCION_TEST_") {
			if err := os.Unsetenv(name); err != nil {
				panic("clearAmbientScionEnv: unset " + name + ": " + err.Error())
			}
		}
	}
}
