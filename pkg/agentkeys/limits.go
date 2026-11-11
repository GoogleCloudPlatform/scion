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

package agentkeys

// Rate limit defaults from .design/agent-keys-contract.md ("Concrete
// defaults"). These are token-bucket parameters for two independent,
// server-owned buckets task 2.2 implements: one per authenticated
// principal+project, one per target agent. Both keys routes (top-level and
// project-scoped) share the same buckets; they are separate from the
// aggregate DM message allowance. Local mode has no Hub quota. These are
// per-Hub-instance limits — there is no distributed quota service.
const (
	// PrincipalProjectRateLimit is the sustained rate, in requests/second,
	// of the per-principal-per-project bucket.
	PrincipalProjectRateLimit = 5
	// PrincipalProjectBurst is that bucket's burst size.
	PrincipalProjectBurst = 10

	// TargetRateLimit is the sustained rate, in requests/second, of the
	// per-target-agent bucket.
	TargetRateLimit = 10
	// TargetBurst is that bucket's burst size.
	TargetBurst = 20
)
