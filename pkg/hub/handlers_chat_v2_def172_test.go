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

package hub

// DEF-172: mentionCoAddressees must populate Addressee.PrincipalID with the
// mentioned agent's slug, not its raw UUID (the same "prefer human-readable
// identity" convention slugify established for the "from" field via
// buildPrincipalRef, but never connected to this separate "to"-field
// construction path). These are direct unit tests of mentionCoAddressees
// itself (pkg/hub/handlers_chat_v2.go:1428), independent of the
// integration-level DEF-169 tests in handlers_chat_v2_def169_test.go which
// exercise the same fix through the real HTTP handler.

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestMentionCoAddressees_UsesSlugNotUUID is also the DEF-172 mutation test:
// reverting mentionCoAddressees's PrincipalID assignment back to ag.ID (the
// pre-fix state) makes this test fail with PrincipalID == "uuid-alpha" /
// "uuid-beta" instead of the slug — exactly the raw-UUID symptom ptone
// reported ("to": ["agent:<uuid>"]) — proving the assertion actually checks
// the slug rather than merely checking that some string is present.
func TestMentionCoAddressees_UsesSlugNotUUID(t *testing.T) {
	agents := []*store.Agent{
		{ID: "uuid-alpha", Slug: "agent-alpha"},
		{ID: "uuid-beta", Slug: "agent-beta"},
	}

	addrs := mentionCoAddressees(agents)

	if len(addrs) != 2 {
		t.Fatalf("len(addrs) = %d, want 2", len(addrs))
	}

	wantSlugs := []string{"agent-alpha", "agent-beta"}
	wantUUIDs := []string{"uuid-alpha", "uuid-beta"}
	for i, addr := range addrs {
		if addr.PrincipalKind != "agent" {
			t.Errorf("addrs[%d].PrincipalKind = %q, want %q", i, addr.PrincipalKind, "agent")
		}
		if addr.PrincipalID != wantSlugs[i] {
			t.Errorf("addrs[%d].PrincipalID = %q, want slug %q", i, addr.PrincipalID, wantSlugs[i])
		}
		if addr.PrincipalID == wantUUIDs[i] {
			t.Errorf("addrs[%d].PrincipalID = %q, want slug — got raw UUID instead (the DEF-172 symptom)", i, addr.PrincipalID)
		}
	}
}

// TestMentionCoAddressees_EmptySlugFallsBackToUUID covers the fallback
// branch: if a *store.Agent somehow carries an empty Slug, PrincipalID must
// fall back to the UUID rather than producing "agent:" with an empty
// suffix. Per the ent schema (pkg/ent/schema/agent.go: field.String("slug").
// NotEmpty()), this is not reachable for a real stored agent record — this
// test exercises the defensive fallback branch directly at the unit level
// since it cannot be reached through the real HTTP handler with a
// store-backed agent.
func TestMentionCoAddressees_EmptySlugFallsBackToUUID(t *testing.T) {
	agents := []*store.Agent{
		{ID: "uuid-no-slug", Slug: ""},
	}

	addrs := mentionCoAddressees(agents)

	if len(addrs) != 1 {
		t.Fatalf("len(addrs) = %d, want 1", len(addrs))
	}
	if addrs[0].PrincipalID != "uuid-no-slug" {
		t.Errorf("PrincipalID = %q, want fallback UUID %q", addrs[0].PrincipalID, "uuid-no-slug")
	}
	if addrs[0].PrincipalID == "" {
		t.Error("PrincipalID is empty — would render as \"agent:\" with no suffix")
	}
}
