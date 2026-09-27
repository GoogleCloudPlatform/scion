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

package projectcompat

// LegacyResourceScope is the legacy name for what is now the "project"
// resource scope (templates, harness configs, skills, and any other
// file-based resource organized by scope).
const LegacyResourceScope = "grove"

// CanonicalResourceScope maps a legacy resource-scope name to its canonical
// form. Already-canonical and unrecognized scope values are returned
// unchanged.
//
// Callers that accept a resource scope from a request (clone or create
// handlers) must call this exactly once, before doing anything that depends
// on the scope value — authorization, storage path resolution, and record
// lookup/create all included. Applying it at a single, early point keeps
// those consumers from ever disagreeing about which scope a request
// resolved to; the caller's own switch over the canonical constants is still
// responsible for rejecting any value this function did not recognize.
func CanonicalResourceScope(scope string) string {
	if scope == LegacyResourceScope {
		return "project"
	}
	return scope
}
