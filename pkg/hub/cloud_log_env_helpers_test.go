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

package hub

import (
	"fmt"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// ambientGCPProjectEnvKeys are the env vars logging.ResolveProjectID reads.
// When either is set, New() builds real Cloud Logging clients unless
// ServerConfig.DisableCloudLogQuery is set (ptone/scion#3188).
var ambientGCPProjectEnvKeys = []string{logging.EnvGCPProjectID, logging.EnvGoogleCloudProject}

// clearAmbientGCPProjectEnv unsets the GCP project env vars for the whole
// test binary, so servers built by helpers that call New() directly (not
// through testServer, which sets DisableCloudLogQuery) do not open outbound
// Cloud Logging connections because of the environment the tests happen to
// run in. Call it once from TestMain, before m.Run(): unlike t.Setenv in a
// shared helper it cannot panic for parallel tests. Tests that need a
// project ID set it themselves with t.Setenv.
func clearAmbientGCPProjectEnv() {
	for _, k := range ambientGCPProjectEnvKeys {
		if err := os.Unsetenv(k); err != nil {
			fmt.Fprintf(os.Stderr, "clearAmbientGCPProjectEnv: unsetting %s: %v\n", k, err)
			os.Exit(1)
		}
	}
}
