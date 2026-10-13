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

var (
	// ErrInvalidWorkloadIssuer marks a malformed workload_issuers entry.
	ErrInvalidWorkloadIssuer = errors.New("invalid test identity workload issuer")
	// ErrWorkloadIssuerAdminTierUnsupported refuses a workload_issuers entry
	// with max_tier "admin": workload delegation covers the member tier only
	// in this release.
	ErrWorkloadIssuerAdminTierUnsupported = errors.New(`test identity workload issuer max_tier "admin" is not supported in this release; only "member" is accepted`)
)

// TestIdentitiesConfig is server.hub.test_identities: startup-only settings
// of hub-issued test identities (ptone/scion#4240).
type TestIdentitiesConfig struct {
	// WorkloadIssuers is the allowlist of workload subjects that a human
	// issuer may pin an issuer delegation to (phase X2). It is read once at
	// startup and is never writable through the API.
	WorkloadIssuers []TestIdentityWorkloadIssuer `json:"workloadIssuers,omitempty" yaml:"workloadIssuers,omitempty" koanf:"workloadIssuers"`
}

// TestIdentityWorkloadIssuer is one workload_issuers entry: a pinned Google
// subject and the highest tier a delegation to it may cover.
type TestIdentityWorkloadIssuer struct {
	// Sub is the Google subject (the service account's unique id) of the
	// workload identity.
	Sub string `json:"sub" yaml:"sub" koanf:"sub"`
	// MaxTier is "member". "admin" is refused at startup until the admin
	// tiers support workload delegation.
	MaxTier string `json:"maxTier" yaml:"maxTier" koanf:"maxTier"`
}

// ValidateTestIdentityWorkloadIssuers checks the workload_issuers allowlist:
// every subject is a non-empty decimal string of at most 64 digits, no
// subject appears twice, and every max_tier is exactly "member". A max_tier
// of "admin" returns ErrWorkloadIssuerAdminTierUnsupported; any other
// problem returns ErrInvalidWorkloadIssuer. The hub refuses to start on
// either.
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
		case WorkloadIssuerTierMember:
		case WorkloadIssuerTierAdmin:
			return fmt.Errorf("%w (%s.workload_issuers[%d].max_tier)", ErrWorkloadIssuerAdminTierUnsupported, TestIdentitiesKey, i)
		default:
			return fmt.Errorf("%w: %s.workload_issuers[%d].max_tier must be %q", ErrInvalidWorkloadIssuer, TestIdentitiesKey, i, WorkloadIssuerTierMember)
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
