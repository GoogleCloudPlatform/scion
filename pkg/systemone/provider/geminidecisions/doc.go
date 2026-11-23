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

// Package geminidecisions implements the System One provider.Adapter for the
// Gemini Developer API (v1beta) Decisions endpoint (POST /v1beta/decisions,
// model "gemini-decision-1"; PV3 brief §§4–5, provider-design §§5–7).
//
// Authentication is performed exclusively in pkg/systemone/egress via the
// adapter-declared header scheme ("header:x-goog-api-key"); this package never
// receives, reads, or constructs credentials or outbound http.Client values.
package geminidecisions
