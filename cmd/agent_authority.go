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

package cmd

import (
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// An agent's authority state (ptone/scion#2143) as the CLI shows it:
// scion list marks an orphaned or revoked agent in the PHASE column, and
// scion look prints the cause on stderr.

// cliShowsAgentAuthority reports whether the CLI asks the Hub for agents'
// authority states and shows them, in the given mode. It is the single
// switch for the mode question: human mode only.
func cliShowsAgentAuthority(mode CLIMode) bool {
	return mode != ModeAgent
}

// knownAuthorityCauses are the causes the CLI prints as they are. Any other
// value from the Hub prints as "unknown", so a Hub string never reaches the
// terminal unchecked.
var knownAuthorityCauses = map[string]bool{
	api.AgentAuthorityCauseDelegatorDeleted:  true,
	api.AgentAuthorityCauseDelegatorInactive: true,
	api.AgentAuthorityCauseDelegatorRevoked:  true,
	api.AgentAuthorityCauseChainInvalid:      true,
	"user_delete_cascade":                    true,
	"agent_soft_delete":                      true,
	"agent_hard_delete":                      true,
	"create_compensation":                    true,
	"reincarnate_replaced":                   true,
	"provenance_adopted":                     true,
	"adoption_reverted":                      true,
	"scope_reissue_replaced":                 true,
}

// authorityMarker returns the PHASE column marker for an agent's authority
// state: "ORPHANED" or "REVOKED", or "" for any other state.
func authorityMarker(a *api.AgentAuthority) string {
	if !a.Flagged() {
		return ""
	}
	return strings.ToUpper(a.State)
}

// authorityPhaseLabel appends the authority marker to a PHASE column value.
func authorityPhaseLabel(phase string, a *api.AgentAuthority) string {
	if m := authorityMarker(a); m != "" {
		return phase + " (" + m + ")"
	}
	return phase
}

// authorityCauseLine returns the line scion look prints for an orphaned or
// revoked agent, or "".
func authorityCauseLine(a *api.AgentAuthority) string {
	m := authorityMarker(a)
	if m == "" {
		return ""
	}
	cause := a.Cause
	if !knownAuthorityCauses[cause] {
		cause = "unknown"
	}
	detail := cause
	if p := authorityPrincipalText(a.Principal); p != "" {
		detail += ", " + p
	}
	return "Authority: " + m + " (" + detail + "); requests that need its delegation are denied. Stop or delete this agent."
}

// authorityPrincipalText describes the principal an authority cause names,
// for example "user deleted" or "agent 0d9f... deleted". The ID is shown
// only when the Hub returned it and it is a plain identifier.
func authorityPrincipalText(p *api.AgentAuthorityPrincipal) string {
	if p == nil {
		return ""
	}
	var kind string
	switch p.Kind {
	case "user", "agent":
		kind = p.Kind
	default:
		return ""
	}
	out := kind
	if p.ID != "" && plainIdentifier(p.ID) {
		out += " " + p.ID
	}
	if p.Deleted {
		out += " deleted"
	}
	return out
}

// plainIdentifier reports whether s holds only letters, digits and '-'.
func plainIdentifier(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return s != ""
}
