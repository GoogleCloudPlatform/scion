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

package config

import "os"

// IsHubManagedAgent reports whether the current process is running as a
// hub-managed agent. The RuntimeBroker sets SCION_AGENT_ID when it starts
// an agent inside hub infrastructure; its presence (and non-empty value)
// is the canonical signal that the CLI is operating in an agent context
// where local-only mode suggestions are inappropriate.
func IsHubManagedAgent() bool {
	return os.Getenv("SCION_AGENT_ID") != ""
}
