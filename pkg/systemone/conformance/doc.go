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

// Package conformance provides the offline httptest conformance harness for
// system-one provider adapters (provider-design §9, §10 PV1, §12; P1 brief §3, §7).
//
// All fixtures executed by this harness are synthetic from published vendor
// documentation and OpenAPI schemas ("synthetic from docs, not recorded from
// the live API"). The harness never contacts external endpoints; every test
// exchange runs in-process over loopback through pkg/systemone/egress's test seam.
package conformance
