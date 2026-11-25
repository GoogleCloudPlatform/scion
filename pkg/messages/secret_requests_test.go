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
		{Key: "LEGACY_VAR_KEY", Type: SecretRequestTypeVariable},
	})
	if err != nil {
		t.Fatalf("NormalizeSecretRequests failed: %v", err)
	}
	if len(reqs) != 3 {
		t.Fatalf("got %d requests, want 3", len(reqs))
	}
	if reqs[0].SecretType != SecretRequestTypeEnvironment {
		t.Errorf("empty type normalized to %q, want %q", reqs[0].SecretType, SecretRequestTypeEnvironment)
	}
	if reqs[1].SecretType != SecretRequestTypeOnDemand {
		t.Errorf("on_demand type normalized to %q, want %q", reqs[1].SecretType, SecretRequestTypeOnDemand)
	}
	if reqs[2].SecretType != SecretRequestTypeOnDemand {
		t.Errorf("legacy variable type normalized to %q, want %q", reqs[2].SecretType, SecretRequestTypeOnDemand)
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
