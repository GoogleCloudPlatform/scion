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

// testServerConfig is DefaultServerConfig with broker auth disabled, for
// tests that exercise handlers without hub keys. DefaultServerConfig enables
// broker auth, and a broker with auth enabled and no keys serves only its
// health endpoints. Use it only where a test serves HTTP from a server with
// no hub keys; tests of the auth path use DefaultServerConfig.
func testServerConfig() ServerConfig {
	cfg := DefaultServerConfig()
	cfg.BrokerAuthEnabled = false
	return cfg
}
