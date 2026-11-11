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
	"os"
	"testing"
)

func TestPinKubernetesNamespace(t *testing.T) {
	t.Setenv("SCION_K8S_NAMESPACE", "leaked-scion-ns")
	t.Setenv("POD_NAMESPACE", "leaked-pod-ns")

	PinKubernetesNamespace()

	if got := os.Getenv("SCION_K8S_NAMESPACE"); got != KubernetesTestNamespace {
		t.Errorf("SCION_K8S_NAMESPACE = %q, want %q", got, KubernetesTestNamespace)
	}
	if got, ok := os.LookupEnv("POD_NAMESPACE"); ok {
		t.Errorf("POD_NAMESPACE = %q, want unset", got)
	}
}
