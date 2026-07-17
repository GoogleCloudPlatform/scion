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

package transportauth

// ADCSource will use Application Default Credentials (google.golang.org/api/idtoken)
// to mint OIDC tokens for workstation CLI usage. Implementation deferred to Phase 5;
// it will live in a subpackage (transportauth/adcsource) so the lean sciontool binary
// never links the Google API client libraries.
//
// The interface it must satisfy:
//
//	type ADCSource struct { ... }
//	func NewADCSource(audience string) (*ADCSource, error)
//	func (s *ADCSource) Token() (string, error)
//	func (s *ADCSource) SetToken(token string, expiry time.Time)
//	func (s *ADCSource) Expiry() time.Time
