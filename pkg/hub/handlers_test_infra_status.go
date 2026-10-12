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
	// TestIdentities is the member tier (--test-mode=member or above).
	TestIdentities bool
	// TestHubAdmin and TestSuperAdmin are the admin tier, read from its
	// immutable startup gate (testAdminTierEnabled, test_admin_gate.go).
	// TestSuperAdmin is true only on a hub started with
	// --test-mode=super-admin (G3).
	TestHubAdmin   bool
	TestSuperAdmin bool
	// TestIdentityWebSessions is true when test identity web sessions are
	// available (ptone/scion#4240, WS1): the member tier is on, the hub
	// has its hub-local web session key, and it is not in proxy auth mode
	// (testIdentityWebSessionGate).
	TestIdentityWebSessions bool
}

// testInfraGates returns the hub's test-identity gates. It is the single
// place GET /api/v1/test-infra/status reads them from.
func (s *Server) testInfraGates() testInfraGates {
	webSessions, _ := s.testIdentityWebSessionGate()
	return testInfraGates{
		TestIdentities:          s.testIdentities.enabled,
		TestHubAdmin:            s.testAdminTierEnabled(testAdminKindHubAdmin),
		TestSuperAdmin:          s.testAdminTierEnabled(testAdminKindSuperAdmin),
		TestIdentityWebSessions: webSessions,
	}
}

// TestInfraStatusResponse is the body of GET /api/v1/test-infra/status:
// exactly four booleans, and no other hub data.
type TestInfraStatusResponse struct {
	TestIdentities          bool `json:"testIdentities"`
	TestHubAdmin            bool `json:"testHubAdmin"`
	TestSuperAdmin          bool `json:"testSuperAdmin"`
	TestIdentityWebSessions bool `json:"testIdentityWebSessions"`
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
