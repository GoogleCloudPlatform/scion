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
	"testing"
)

// X2 AC (allowlist): the startup allowlist accepts only decimal subjects
// with maxTier "member"; "admin" refuses startup with its own named error.
func TestTestIdentityWorkloadIssuers_Validate(t *testing.T) {
	ok := []TestIdentityWorkloadIssuer{{Sub: "100000000000000000001", MaxTier: "member"}, {Sub: "2", MaxTier: "member"}}
	if err := ValidateTestIdentityWorkloadIssuers(ok); err != nil {
		t.Fatalf("valid allowlist refused: %v", err)
	}
	if err := ValidateTestIdentityWorkloadIssuers(nil); err != nil {
		t.Fatalf("empty allowlist refused: %v", err)
	}

	admin := []TestIdentityWorkloadIssuer{{Sub: "1", MaxTier: "admin"}}
	if err := ValidateTestIdentityWorkloadIssuers(admin); !errors.Is(err, ErrWorkloadIssuerAdminTierUnsupported) {
		t.Fatalf("admin maxTier: err = %v, want ErrWorkloadIssuerAdminTierUnsupported", err)
	}

	long := make([]byte, 65)
	for i := range long {
		long[i] = '1'
	}
	bad := map[string][]TestIdentityWorkloadIssuer{
		"empty sub":          {{Sub: "", MaxTier: "member"}},
		"non-decimal sub":    {{Sub: "sa@example.iam.gserviceaccount.com", MaxTier: "member"}},
		"space in sub":       {{Sub: "12 3", MaxTier: "member"}},
		"sub too long":       {{Sub: string(long), MaxTier: "member"}},
		"duplicate sub":      {{Sub: "7", MaxTier: "member"}, {Sub: "7", MaxTier: "member"}},
		"empty tier":         {{Sub: "7", MaxTier: ""}},
		"case variant tier":  {{Sub: "7", MaxTier: "Member"}},
		"unknown tier":       {{Sub: "7", MaxTier: "super-admin"}},
		"viewer is no tier":  {{Sub: "7", MaxTier: "viewer"}},
		"good then bad tier": {{Sub: "7", MaxTier: "member"}, {Sub: "8", MaxTier: "x"}},
	}
	for name, list := range bad {
		t.Run(name, func(t *testing.T) {
			if err := ValidateTestIdentityWorkloadIssuers(list); !errors.Is(err, ErrInvalidWorkloadIssuer) {
				t.Fatalf("err = %v, want ErrInvalidWorkloadIssuer", err)
			}
		})
	}
}

// server.hub.test_identities.workload_issuers loads from the settings file
// into HubServerConfig.TestIdentities, and is absent by default.
func TestTestIdentityWorkloadIssuers_LoadFromSettingsFile(t *testing.T) {
	yaml := "schema_version: \"1\"\nserver:\n  hub:\n    test_identities:\n      workload_issuers:\n        - sub: \"100000000000000000001\"\n          max_tier: member\n"
	scionDir, _ := writeConfigPathFixture(t, yaml, "schema_version: \"1\"\n")
	gc, err := LoadGlobalConfig(scionDir)
	if err != nil {
		t.Fatal(err)
	}
	got := gc.Hub.TestIdentityWorkloadIssuers()
	if len(got) != 1 || got[0].Sub != "100000000000000000001" || got[0].MaxTier != "member" {
		t.Fatalf("workload issuers = %+v", got)
	}

	scionDir, _ = writeConfigPathFixture(t, "schema_version: \"1\"\n", "schema_version: \"1\"\n")
	gc, err = LoadGlobalConfig(scionDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := gc.Hub.TestIdentityWorkloadIssuers(); got != nil {
		t.Fatalf("default workload issuers = %+v, want nil", got)
	}
}
