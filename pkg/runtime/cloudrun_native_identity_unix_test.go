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

//go:build unix

package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// SCION_INIT_FIREWALL supplied only by the harness env, from GetEnv or
// GetTelemetryEnv, reaches the final instance env unchanged (the harness
// env does not deny it). On a native "assign" instance Run then refuses
// it, on or invalid, before any Instances API call, so no instance is
// created. Native "block" and an instance without a native identity run
// unchanged, and the harness value reaches the created instance.
func TestCloudRunRun_HarnessInitFirewall(t *testing.T) {
	identities := []struct {
		label string
		id    *api.CloudRunIdentity
	}{
		{label: "identity-assign", id: &api.CloudRunIdentity{ServiceAccount: nativeIDAssignedSA, Mode: "assign"}},
		{label: "identity-block", id: &api.CloudRunIdentity{ServiceAccount: nativeIDBlockSA, Mode: "block"}},
		{label: "identity-unset"},
	}
	for _, ident := range identities {
		for _, source := range []string{"getenv", "telemetry"} {
			for _, value := range []string{"true", "garbage"} {
				t.Run(ident.label+"/"+source+"/"+value, func(t *testing.T) {
					supplied := map[string]string{CloudRunInitFirewallEnv: value}
					h := newCREnvHarness(supplied, nil)
					if source == "telemetry" {
						h = newCREnvHarness(nil, supplied)
					}
					fake := &fakeInstancesClient{getErr: notFoundErr()}
					rt, _ := nativeIdentityRuntime(t, fake)
					provisioned := crCountProvisionNFS(rt)
					cfg := nativeIdentityRunConfig(ident.id)
					cfg.Harness = h
					cfg.TelemetryEnabled = true

					_, err := rt.Run(context.Background(), cfg)
					if ident.id == nil || ident.id.Mode != "assign" {
						if err != nil {
							t.Fatalf("Run: %v", err)
						}
						if got, ok := crInstanceEnv(t, fake)[CloudRunInitFirewallEnv]; !ok || got != value {
							t.Errorf("%s = %q (present %v), want the harness value %q", CloudRunInitFirewallEnv, got, ok, value)
						}
						return
					}
					if err == nil {
						t.Fatal("Run succeeded, want the init firewall refusal")
					}
					want := fmt.Sprintf("agent %q: %s", "agent-1", CloudRunInitFirewallRefusal)
					if value == "garbage" {
						want = fmt.Sprintf("agent %q: the %s value is invalid; %s", "agent-1", CloudRunInitFirewallEnv, CloudRunInitFirewallRefusal)
					}
					if err.Error() != want {
						t.Errorf("Run error = %q, want %q", err.Error(), want)
					}
					if strings.Contains(err.Error(), nativeIDAssignedSA) {
						t.Errorf("Run error %q names the account", err.Error())
					}
					if *provisioned != 0 {
						t.Errorf("provisioned %d times before the refusal, want none", *provisioned)
					}
					if len(fake.getReqs)+len(fake.createReqs)+len(fake.startReqs)+len(fake.deleteReqs) != 0 {
						t.Errorf("Instances API called (get %d, create %d, start %d, delete %d), want none",
							len(fake.getReqs), len(fake.createReqs), len(fake.startReqs), len(fake.deleteReqs))
					}
				})
			}
		}
	}
}
