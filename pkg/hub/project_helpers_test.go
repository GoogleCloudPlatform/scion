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

import (
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// expectedResolvedWrapperFields and expectedResolvedEntryFields are the JSON
// keys the resolved-settings response is allowed to carry, at the top level and
// per setting respectively. They live at package scope because two files assert
// against them: this one exact-sets the marshalled types, and
// project_settings_resolved_test.go derives its wire-level length check from
// the wrapper list rather than hardcoding a number that can silently fall out
// of step with it.
//
// Both must stay sorted; the assertions compare sorted slices.
var (
	expectedResolvedWrapperFields = []string{"project", "settings"}
	expectedResolvedEntryFields   = []string{"hubDefault", "hubValue", "projectSet", "projectValue"}
)

// resolvedResponseTypes enumerates every type whose fields land directly in the
// resolved-settings JSON object. There are exactly four, and they are listed
// rather than described, because a rule stated as "all the response types" is
// not checkable — the next reader cannot tell whether a type they are looking
// at is in the set:
//
//   - hub.ResolvedProjectSettings        — the wrapper the handler marshals
//   - hub.ResolvedProjectSetting         — one entry in its Settings map
//   - hubclient.ResolvedProjectSettings  — the client mirror of the wrapper
//   - hubclient.ResolvedProjectSetting   — the client mirror of the entry
//
// hubclient.ProjectSettings is deliberately NOT here. It is carried whole
// behind the "project" key rather than promoted into this object, it has
// sixteen fields that change for unrelated reasons, and it legitimately uses
// omitempty throughout. The wire-level denylist in
// project_settings_resolved_test.go is what looks inside it.
//
// COVERAGE ARGUMENT — why the checks applied to these four are the set they
// are. This guard reads the keys encoding/json emits for ONE constructed value.
// That is a bound on real responses only if nothing can suppress a key for that
// value while emitting it for another. There are THREE ways that can happen,
// and the enumeration is the whole argument — "we handled the obvious ones" is
// not a soundness claim:
//
//  1. a TAG OPTION suppresses the field when it is zero. `omitempty` does
//     this; so does `omitzero` (Go 1.24+), which was missed for exactly as long
//     as this check named `omitempty` alone. Stated as a category, and enforced
//     with an allowlist, because the language has grown one of these twice.
//     -> assertOnlySafeTagOptions
//  2. a nil EMBEDDED STRUCT POINTER drops every field it promotes, with no
//     tag and no marshaller involved.
//     -> assertNoEmbeddedStructPointer
//  3. a custom MarshalJSON declines to write the key.
//     -> assertNoCustomMarshaler
//
// Close all three and a zero-value marshal is PROVABLY complete, which is a
// stronger claim than the incidental completeness this guard had before.
//
// Each was measured, and the list reached three by being wrong at two. Route 2
// was found by review after the first version of this argument shipped claiming
// the enumeration was complete at two:
//
//	type shape struct { ProjectSet bool `json:"projectSet"`; *embed }
//	type embed struct { Winner string `json:"winner"` }   // note: no omitempty
//
//	ZERO      -> {"projectSet":false}
//	POPULATED -> {"projectSet":true,"winner":"sneaked"}
//
// Invisible to an omitempty ban (no tag to find), invisible to a marshaller ban
// (no marshaller), and green under the exact-set assertion. A NAMED pointer
// field is fine and is not what this rejects: `Extra *embed json:"extra"`
// marshals to {"projectSet":false,"extra":null}, because it nests under a key
// instead of promoting.
//
// WHY PROHIBITION RATHER THAN A POPULATED FIXTURE. Two different things get
// called "just populate the value first", and only one of them is ruled out:
//
//   - Filling REFLECTIVELY cannot reach route 2. FieldByName through a nil
//     embedded pointer panics outright with "reflect: indirection through nil
//     pointer to embedded struct", and no single traversal handles embedded
//     values and embedded pointers both.
//   - Asserting on a fully-populated HAND-CONSTRUCTED value does work, on all
//     three routes. Nothing reflects, so the embedded pointer is simply non-nil
//     because the literal made it so. This is a real alternative and it is not
//     what the comment above should be read as excluding.
//
// Prohibition is chosen over that alternative on DRIFT, not on reach. A
// populated fixture is a hand-maintained copy of the type: add a field, forget
// to populate it, and the guard silently goes blind with nothing failing. That
// is the same defect class the descriptor derivation in this commit removes, so
// spending it here to save it there would be a wash at best. The prohibition is
// structural — it cannot rot, because there is nothing to keep in sync. Its
// real cost is that it constrains future code and cannot express a
// legitimately-conditional key; if that day comes, the populated fixture is the
// thing to reach for, and this paragraph is why.
//
// WHAT THE WALKS DO WITH EACH ANONYMOUS CASE. The three differ, and one loop
// that treats them alike is wrong:
//
//   - embedded VALUE struct  -> RECURSE. Its fields promote, so its tags and
//     its own embedded fields are in scope.
//   - embedded struct POINTER -> REJECT, and never dereference. Dereferencing
//     would inspect the target's tags, find them clean, and pass the shape.
//   - embedded INTERFACE (exported) -> LEAVE. It does not flatten; it becomes a
//     stable named key. Measured: {"projectSet":false,"PIface":null} zero,
//     {"projectSet":true,"PIface":{...}} populated. The value changes, the key
//     does not, and the key is all the exact-set assertion reads.
//
// The recursion is load-bearing, because route 2 is depth-recursive and hides
// one level under an embedded value, where a top-level scan does not look:
//
//	type deep struct { Winner string `json:"winner"` }
//	type mid  struct { *deep }                                  // pointer
//	type outer struct { ProjectSet bool `json:"projectSet"`; mid } // VALUE
//
//	ZERO      -> {"projectSet":false}
//	POPULATED -> {"projectSet":true,"winner":"sneaked"}
//
// outer's only anonymous field is `mid`, a struct VALUE — so a check phrased as
// "no anonymous pointer fields on the four types" passes this. Measured against
// assertNoEmbeddedStructPointer, it is rejected at path "mid.deep".
// IT IS A VAR, NOT A FUNC, DELIBERATELY. TestResolvedSettingsGuard_InterveningIsWiredToTheResponseTypes
// swaps it to prove the shape guard actually applies its interveners to whatever
// this returns. Do not turn it back into a func, and do not hardcode this list at
// the call sites: both break that closure, and hardcoding also stops the guard
// growing as the type list grows, which is the regression the swap exists to catch.
var resolvedResponseTypes = func() []any {
	return []any{
		ResolvedProjectSettings{},
		ResolvedProjectSetting{},
		hubclient.ResolvedProjectSettings{},
		hubclient.ResolvedProjectSetting{},
	}
}

// marshalerCtrlPointer carries a POINTER-receiver MarshalJSON. Only *T satisfies
// json.Marshaler; T does not. This is the shape a value-receiver-only check misses.
type marshalerCtrlPointer struct {
	X int `json:"x"`
}

// Control fixtures for TestResolvedSettingsGuard_InstrumentControls. Each is the
// smallest type that exhibits one shape. They are deliberately NOT variations on
// the real response types: a control that resembles the thing it guards invites
// someone to "fix" the control when the real type changes.
type (
	ctlClean struct {
		ProjectSet bool `json:"projectSet"`
	}
	ctlEmbedTarget struct {
		Winner string `json:"winner"`
	}
	ctlOmitTarget struct {
		Winner string `json:"winner,omitempty"`
	}
	ctlZeroTarget struct {
		Winner string `json:"winner,omitzero"`
	}
)

// ctlOmitZero is the shape that was green at 25f82c33 while putting a key on the
// wire. It is the reason the tag check became an allowlist.
type ctlOmitZero struct {
	ProjectSet bool `json:"projectSet"`
	ctlZeroTarget
}

// ctlNestedPtr hides the embedded pointer one level down, under an embedded
// VALUE. A top-level scan for anonymous pointer fields does not see it.
type ctlNestedPtr struct {
	ProjectSet bool `json:"projectSet"`
	ctlMid
}

func (*marshalerCtrlPointer) MarshalJSON() ([]byte, error) { return []byte(`{"custom":1}`), nil }

type ctlMid struct{ *ctlEmbedTarget }
