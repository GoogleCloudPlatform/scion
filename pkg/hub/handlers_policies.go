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

// =============================================================================
// CO1 Cutover: Policy API removed
// =============================================================================
//
// All policy CRUD and binding endpoints were removed in the CO1 evaluator
// cutover. Authorization is now handled entirely by RoleBindings and the
// AK1 kernel. Existing Policy rows in the database are dead data.
//
// The handler functions below return 410 Gone so that clients get a clear
// signal that the endpoints have been retired.

func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "Policy API removed in CO1 cutover. Use Role Bindings.", http.StatusGone)
}

func (s *Server) handlePolicyRoutes(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "Policy API removed in CO1 cutover. Use Role Bindings.", http.StatusGone)
}
