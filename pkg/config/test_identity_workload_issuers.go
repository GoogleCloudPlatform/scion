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

import (
	"errors"
	"fmt"
)

// TestIdentitiesKey is the settings key of the test identity startup
// settings (HubServerConfig.TestIdentities). It is file/env only: the
// server-config API refuses it, and every key under it, in every mode.
const TestIdentitiesKey = "server.hub.test_identities"

// Workload issuer tiers. A workload issuer entry's max_tier is the highest
// test identity tier a delegation pinned to that subject may cover.
const (
	WorkloadIssuerTierMember = "member"
	WorkloadIssuerTierAdmin  = "admin"
)

// workloadIssuerSubMaxLen bounds a workload subject. A Google service
// account's unique id is a decimal string of about 21 digits.
const workloadIssuerSubMaxLen = 64

// ErrInvalidWorkloadIssuer marks a malformed workload_issuers entry.
var ErrInvalidWorkloadIssuer = errors.New("invalid test identity workload issuer")

// TestIdentitiesConfig is server.hub.test_identities: startup-only settings
// of hub-issued test identities (ptone/scion#4240).
type TestIdentitiesConfig struct {
	// WorkloadIssuers is the allowlist of workload subjects that a human
	// issuer may pin an issuer delegation to (phase X2). It is read once at
	// startup and is never writable through the API.
	WorkloadIssuers []TestIdentityWorkloadIssuer `json:"workloadIssuers,omitempty" yaml:"workloadIssuers,omitempty" koanf:"workloadIssuers"`
	// AdminWebSessions turns on browser web sessions for the admin test
	// identity kinds (phase WS4). Its value is the hub id the setting is
	// bound to, and it must equal this hub's resolved (persistent) hub id;
	// empty means off. It is read once at startup, is never writable
	// through the API, and is refused at startup unless --test-mode is
	// admin or super-admin.
	AdminWebSessions string `json:"adminWebSessions,omitempty" yaml:"adminWebSessions,omitempty" koanf:"adminWebSessions"`
}

// TestIdentityWorkloadIssuer is one workload_issuers entry: a pinned Google
// subject and the highest tier a delegation to it may cover.
type TestIdentityWorkloadIssuer struct {
	// Sub is the Google subject (the service account's unique id) of the
	// workload identity.
	Sub string `json:"sub" yaml:"sub" koanf:"sub"`
	// MaxTier is "member" (member and viewer issuance) or "admin" (also the
	// admin tiers, phase X4). The hub refuses to start with an "admin"
	// entry unless it runs with --test-mode=admin or above.
	MaxTier string `json:"maxTier" yaml:"maxTier" koanf:"maxTier"`
}

// ValidateTestIdentityWorkloadIssuers checks the workload_issuers allowlist:
// every subject is a non-empty decimal string of at most 64 digits, no
// subject appears twice, and every max_tier is exactly "member" or
// "admin". Any problem returns ErrInvalidWorkloadIssuer, and the hub
// refuses to start on it. Whether an "admin" entry is admitted at the
// hub's test mode is the hub's own startup check.
func ValidateTestIdentityWorkloadIssuers(issuers []TestIdentityWorkloadIssuer) error {
	seen := make(map[string]bool, len(issuers))
	for i, e := range issuers {
		if !isDecimalSubject(e.Sub) {
			return fmt.Errorf("%w: %s.workload_issuers[%d].sub must be 1 to %d decimal digits", ErrInvalidWorkloadIssuer, TestIdentitiesKey, i, workloadIssuerSubMaxLen)
		}
		if seen[e.Sub] {
			return fmt.Errorf("%w: %s.workload_issuers[%d].sub repeats an earlier entry", ErrInvalidWorkloadIssuer, TestIdentitiesKey, i)
		}
		seen[e.Sub] = true
		switch e.MaxTier {
		case WorkloadIssuerTierMember, WorkloadIssuerTierAdmin:
		default:
			return fmt.Errorf("%w: %s.workload_issuers[%d].max_tier must be %q or %q", ErrInvalidWorkloadIssuer, TestIdentitiesKey, i, WorkloadIssuerTierMember, WorkloadIssuerTierAdmin)
		}
	}
	return nil
}

// workloadIssuers returns the configured workload_issuers allowlist, or nil.
func (c *TestIdentitiesConfig) workloadIssuers() []TestIdentityWorkloadIssuer {
	if c == nil {
		return nil
	}
	return c.WorkloadIssuers
}

// TestIdentityAdminWebSessionsKey is the settings key of the admin web
// session binding (TestIdentitiesConfig.AdminWebSessions).
const TestIdentityAdminWebSessionsKey = TestIdentitiesKey + ".admin_web_sessions"

// TestIdentityAdminWebSessions returns the hub id the admin web session
// setting (server.hub.test_identities.admin_web_sessions) is bound to, or
// "" when it is not set.
func (h HubServerConfig) TestIdentityAdminWebSessions() string {
	if h.TestIdentities == nil {
		return ""
	}
	return h.TestIdentities.AdminWebSessions
}

// TestIdentityWorkloadIssuers returns the hub's workload_issuers allowlist
// (server.hub.test_identities.workload_issuers), or nil when none is set.
func (h HubServerConfig) TestIdentityWorkloadIssuers() []TestIdentityWorkloadIssuer {
	return h.TestIdentities.workloadIssuers()
}

func isDecimalSubject(s string) bool {
	if s == "" || len(s) > workloadIssuerSubMaxLen {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
