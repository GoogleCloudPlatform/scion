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

package messages

import (
	"strings"
	"testing"
)

func TestNormalizeSecretRequests_OnDemandAndLegacyVariable(t *testing.T) {
	reqs, err := NormalizeSecretRequests("secret_requests", []SecretRequestInput{
		{Key: "DEFAULT_KEY"},
		{Key: "ON_DEMAND_KEY", Type: SecretRequestTypeOnDemand},
		{Key: "ON_DEMAND_SAME_TARGET", Type: SecretRequestTypeOnDemand, Target: "ON_DEMAND_SAME_TARGET"},
		{Key: "LEGACY_VAR_KEY", Type: "variable"},
		{Key: "ENV_KEY", Type: SecretRequestTypeEnvironment},
	})
	if err != nil {
		t.Fatalf("NormalizeSecretRequests failed: %v", err)
	}
	if len(reqs) != 5 {
		t.Fatalf("got %d requests, want 5", len(reqs))
	}
	if reqs[0].SecretType != SecretRequestTypeOnDemand {
		t.Errorf("empty type normalized to %q, want %q", reqs[0].SecretType, SecretRequestTypeOnDemand)
	}
	if reqs[1].SecretType != SecretRequestTypeOnDemand || reqs[1].Target != "" {
		t.Errorf("on_demand normalized to (%q, target=%q), want (%q, \"\")", reqs[1].SecretType, reqs[1].Target, SecretRequestTypeOnDemand)
	}
	if reqs[2].SecretType != SecretRequestTypeOnDemand || reqs[2].Target != "" {
		t.Errorf("on_demand with target==key normalized to (%q, target=%q), want (%q, \"\")", reqs[2].SecretType, reqs[2].Target, SecretRequestTypeOnDemand)
	}
	if reqs[3].SecretType != SecretRequestTypeOnDemand {
		t.Errorf("legacy variable type normalized to %q, want %q", reqs[3].SecretType, SecretRequestTypeOnDemand)
	}
	if reqs[4].SecretType != SecretRequestTypeEnvironment {
		t.Errorf("environment type normalized to %q, want %q", reqs[4].SecretType, SecretRequestTypeEnvironment)
	}

	// Non-key target is rejected for on_demand (both explicit and default).
	for _, tc := range []SecretRequestInput{
		{Key: "OD_BAD_TARGET", Type: SecretRequestTypeOnDemand, Target: "OTHER_TARGET"},
		{Key: "DEFAULT_BAD_TARGET", Target: "OTHER_TARGET"},
		{Key: "LEGACY_BAD_TARGET", Type: "variable", Target: "OTHER_TARGET"},
	} {
		_, err = NormalizeSecretRequests("secret_requests", []SecretRequestInput{tc})
		if err == nil {
			t.Errorf("expected error for %+v with non-key target", tc)
		}
	}

	_, err = NormalizeSecretRequests("secret_requests", []SecretRequestInput{
		{Key: "BAD_TYPE_KEY", Type: "bogus"},
	})
	if err == nil {
		t.Fatal("expected error for invalid secret request type")
	}
	if !strings.Contains(err.Error(), "on_demand") {
		t.Errorf("error %q should list on_demand as an allowed type", err.Error())
	}
}
