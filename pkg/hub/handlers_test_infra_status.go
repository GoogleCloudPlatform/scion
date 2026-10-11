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

import "net/http"

// testInfraGates is the set of test-identity gates a hub runs with. Each
// value is fixed when the server is built from its startup flags; nothing
// changes it at runtime. The web client shows a test-hub banner whenever
// any of them is on (ptone/scion#4240, phase W).
type testInfraGates struct {
	// TestIdentities is the member tier (--enable-test-identities).
	TestIdentities bool
	// TestHubAdmin and TestSuperAdmin are the admin tier. The admin tier is
	// not built yet, so both are false; when it lands, it fills them in
	// here from its own startup gate value, and nothing else changes.
	TestHubAdmin   bool
	TestSuperAdmin bool
}

// testInfraGates returns the hub's test-identity gates. It is the single
// place GET /api/v1/test-infra/status reads them from.
func (s *Server) testInfraGates() testInfraGates {
	return testInfraGates{
		TestIdentities: s.testIdentities.enabled,
		TestHubAdmin:   false,
		TestSuperAdmin: false,
	}
}

// TestInfraStatusResponse is the body of GET /api/v1/test-infra/status:
// exactly three booleans, and no other hub data.
type TestInfraStatusResponse struct {
	TestIdentities bool `json:"testIdentities"`
	TestHubAdmin   bool `json:"testHubAdmin"`
	TestSuperAdmin bool `json:"testSuperAdmin"`
}

// handleTestInfraStatus handles GET /api/v1/test-infra/status.
//
// The route is public and read-only, so the web client can show the
// test-hub banner on every page, the login page included, for every user.
// On a hub without test identities it returns all false.
func (s *Server) handleTestInfraStatus(w http.ResponseWriter, _ *http.Request) {
	// A conversion, so the two types must keep the same fields in the
	// same order; a field added to either one stops this compiling.
	writeJSON(w, http.StatusOK, TestInfraStatusResponse(s.testInfraGates()))
}
