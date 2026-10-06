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

//go:build !no_sqlite

package hub

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// TestTestServerBuildsNoCloudLogClient pins ptone/scion#3188: even with a
// GCP project env var set, the shared testServer helper builds no Cloud
// Logging query service (and therefore no real Cloud Logging clients),
// because it sets ServerConfig.DisableCloudLogQuery. Not parallel: it uses
// t.Setenv.
func TestTestServerBuildsNoCloudLogClient(t *testing.T) {
	for _, key := range ambientGCPProjectEnvKeys {
		t.Run(key, func(t *testing.T) {
			for _, k := range ambientGCPProjectEnvKeys {
				t.Setenv(k, "")
			}
			t.Setenv(key, "test-ambient-project")
			if got := logging.ResolveProjectID(); got != "test-ambient-project" {
				t.Fatalf("ResolveProjectID() = %q, want the env value; the gate under test would not be reached", got)
			}

			srv, _ := testServer(t)
			if srv.logQueryService != nil {
				t.Fatalf("testServer built a Cloud Logging query service with %s set; want none", key)
			}
		})
	}
}

// TestTestMainClearsAmbientGCPProjectEnv pins the TestMain half of
// ptone/scion#3188: helpers that call New() directly with a default config
// must not see an ambient GCP project ID either.
func TestTestMainClearsAmbientGCPProjectEnv(t *testing.T) {
	if got := logging.ResolveProjectID(); got != "" {
		t.Fatalf("ResolveProjectID() = %q inside the test binary; TestMain must clear the ambient GCP project env", got)
	}
}
