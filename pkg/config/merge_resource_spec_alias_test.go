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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestMergeResourceSpec_ReturnsBaseWhenOverrideNil guards the aliasing hazard
// that the defensive copy in pkg/agent/provision.go's hub agent_defaults block
// (`base := *hd.Resources`) exists to avoid, and it
// works by inverting the usual direction: it does not assert that the copy is
// present, it asserts that the copy is still NECESSARY.
//
// The hazard: MergeResourceSpec returns its base argument ITSELF — the same
// pointer, not a clone — when override is nil. The hub agent_defaults tier
// passes the context's *api.ResourceSpec in base position, so without a copy
// first it would publish that pointer into the agent's persisted config.
//
// WHY THIS TEST LIVES IN pkg/config AND NOT NEXT TO THE CODE IT PROTECTS.
// This is deliberate and it must not be "tidied up" by moving it into
// pkg/agent's Resources test family. Aliasing is a POINTER IDENTITY property,
// and ProvisionAgent reloads its config from disk near its end
// (`finalScionCfg = updatedCfg`). That round trip PRESERVES VALUES AND DESTROYS
// IDENTITY. So any version of this test routed through ProvisionAgent — or
// asserting on the written scion-agent.json, whose serialization launders
// identity the same way — passes whether or not the defensive copy exists. It
// was tried and measured; it cannot discriminate from there. Calling
// MergeResourceSpec directly is the only position from which identity survives
// to be asserted.
//
// This test is NOT covered by pkg/agent's
// TestProvision_HubAgentDefaults_Resources, and that test is not covered by
// this one. That one asserts the hub-vs-broker RANK (values, end to end, which
// survive the reload); this one asserts POINTER IDENTITY at the merge itself.
// Removing either leaves a real hole that the other's green does not report.
func TestMergeResourceSpec_ReturnsBaseWhenOverrideNil(t *testing.T) {
	base := &api.ResourceSpec{Limits: api.ResourceList{CPU: "3"}}
	if got := MergeResourceSpec(base, nil); got != base {
		t.Fatal("MergeResourceSpec no longer returns its base argument when override is nil. " +
			"The defensive copy in pkg/agent/provision.go (base := *hd.Resources) exists solely because it did; " +
			"re-derive whether it is still needed before removing it.")
	}
}

// TestMergeResourceSpec_CopiesWhenBaseNil pins the other half of the asymmetry,
// because the asymmetry is the reason the hazard is easy to miss: an argument in
// OVERRIDE position is copied, an argument in BASE position is not. A reader who
// checks only the base-nil branch sees a function that clones and concludes the
// defensive copy above is redundant.
func TestMergeResourceSpec_CopiesWhenBaseNil(t *testing.T) {
	override := &api.ResourceSpec{Limits: api.ResourceList{CPU: "3"}}
	got := MergeResourceSpec(nil, override)
	if got == override {
		t.Fatal("MergeResourceSpec now returns its override argument itself when base is nil; " +
			"callers passing a shared spec in override position need a defensive copy too")
	}
	if got == nil || got.Limits.CPU != "3" {
		t.Fatalf("value not carried through: %+v", got)
	}
}
