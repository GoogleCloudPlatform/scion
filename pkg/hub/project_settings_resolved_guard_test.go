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
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These guards deliberately carry no build tag. They assert on the shape of the
// API contract, which does not vary by build configuration, and a guard that
// only runs in some configurations is a guard that can be missed in the others.
//
// That decision is no longer just an argument. CI runs `make test-fast`, which
// is `-tags no_sqlite`, and project_settings_resolved_test.go carries
// `//go:build !no_sqlite` — so every behaviour test for this endpoint is
// invisible to CI, and the tests in THIS file are the only ones that run there.
// At the time of writing that is 10 of 20 invisible.
//
// Measure it, do not count the funcs by eye:
//
//	go test -count=1 -list 'TestResolvedSettings' ./pkg/hub/
//	go test -count=1 -tags no_sqlite -list 'TestResolvedSettings' ./pkg/hub/
//
// Two instruments give the wrong answer here and both are tempting. Grepping
// for `func Test` counts tests the compiler never sees. Reading the top of the
// file for a build tag reports "untagged" for BOTH files, because the tag sits
// at line 15 underneath a 14-line licence header — long enough to hide it from
// anything that only looks at the first few lines.
//
// The wider coverage gap is task #17, deliberately not patched over here; it is
// named so nobody reads the untagged choice in this file as an accident.

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
	expectedResolvedEntryFields   = []string{"hubDefault", "projectSet", "projectValue"}
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

// assertNoEmbeddedStructPointer fails if v, or anything embedded in it, has an
// anonymous pointer-to-struct field.
//
// This is intervener 2, and it is the one that was missing from the first
// version of this argument. An embedded struct POINTER promotes its fields onto
// the parent object exactly like an embedded value does — but when the pointer
// is nil, encoding/json emits none of them. No `omitempty` is involved and no
// custom marshaller is involved, so the other two checks are both blind to it,
// and a guard reading a zero value sees a clean object:
//
//	ZERO      -> {"projectSet":false}
//	POPULATED -> {"projectSet":true,"winner":"sneaked"}
//
// A NAMED pointer field is fine and is not rejected here: it nests under its
// own key and marshals to `null` when nil, so the key is still present and the
// exact-set assertion still sees it. The problem is specific to promotion.
//
// Rejecting rather than handling is deliberate. A fill cannot rescue this shape
// — FieldByName through a nil embedded pointer panics with "reflect:
// indirection through nil pointer to embedded struct" — and there is no single
// reflective traversal that copes with embedded values and embedded pointers
// both. When no traversal is sound, prohibiting the shape is the only honest
// move left.
func assertNoEmbeddedStructPointer(t *testing.T, v any) {
	t.Helper()

	typ := reflect.TypeOf(v)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	require.Equal(t, reflect.Struct, typ.Kind())

	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag := field.Tag.Get("json")
			if tag == "-" {
				continue
			}
			if !field.Anonymous || strings.Split(tag, ",")[0] != "" {
				continue // named field: nests under a key, always emitted
			}

			assert.NotEqualf(t, reflect.Pointer, field.Type.Kind(),
				"%s%s is an embedded struct POINTER, which is not allowed on the "+
					"resolved-settings response types.\n"+
					"Its fields are promoted onto this object, so when it is nil the "+
					"encoder emits none of them and the exact-set shape guard — which "+
					"reads a zero value — sees nothing missing.\n"+
					"This fires on the SHAPE, whatever the promoted fields are tagged "+
					"with. In the canonical case no omitempty and no custom marshaller "+
					"are involved, so neither of the other two checks can see it. If the "+
					"target DOES carry omitempty, this check simply reports it first — "+
					"the walk rejects the pointer instead of descending through it, so "+
					"the message says \"embedded pointer\" and not \"omitempty\". The "+
					"verdict is correct either way; it is not a false positive.\n"+
					"Give it a name and a json tag if you need it: a named pointer nests "+
					"under its own key and marshals to null, which the guard can see.",
				path, field.Name)

			if field.Type.Kind() == reflect.Struct {
				walk(field.Type, path+field.Name+".")
			}
		}
	}
	walk(typ, "")
}

// assertNoCustomMarshaler fails if v implements json.Marshaler.
//
// A custom marshaller can emit a key conditionally — on a field's value, on the
// time of day, on anything — so no guard that inspects a test-constructed value
// can bound what it writes. Measured on the real type, a marshaller emitting
// "winner" only when ProjectSet was true left every guard in this file green
// while the field reached the wire in production.
//
// The prohibition is cheap because these are plain data types with no reason to
// hand-roll encoding. If one ever genuinely needs a custom marshaller, this
// assertion is the right place to stop and reconsider: the shape guard would
// have to be rewritten to drive the marshaller across enough inputs to
// enumerate its outputs, which is a much larger and less reliable test than
// this one.
// implementsJSONMarshaler reports whether EITHER receiver form of typ carries a
// custom MarshalJSON.
//
// THE TWO HALVES COVER DISJOINT INPUTS; NEITHER IS BELT-AND-BRACES. Measured:
//
//	input typ            value half   pointer half
//	T (value type)         false*        true       *false iff receiver is *T
//	*T (pointer type)      true          FALSE
//
// For a VALUE type the value half is strictly implied by the pointer half — a
// value-receiver method is in *T's method set too — so it catches nothing the
// other misses. For a POINTER type the implication reverses and the pointer half
// goes false, because PointerTo(*T) is **T, whose method set is empty. Each half
// is therefore the only working half for one of the two input kinds.
//
// Today every caller passes a value, so the VALUE half is unreachable-but-not-
// wrong; it becomes the only thing that works the moment someone writes
// assertNoCustomMarshaler(t, &ResolvedProjectSettings{}). Both are pinned below
// rather than only the one currently doing work, because "this half is dead" is
// a fact about the call sites, not about the function.
//
// WHY THE POINTER HALF MATTERS FOR THIS ENDPOINT SPECIFICALLY. A pointer-receiver
// MarshalJSON is invoked by encoding/json only on an addressable value, so
// json.Marshal(T{}) does not call it and json.Marshal(&T{}) does. That split is
// not hypothetical here: the handler marshals a POINTER — resolvedProjectSettings
// returns *ResolvedProjectSettings and writeJSON is handed that pointer directly
// — while jsonWireNames below marshals a VALUE. So a value-receiver-only check
// permits precisely the form production uses, and the guard would read a clean
// zero-value object while the handler emitted whatever the marshaller wrote.
//
// This is factored out of the assertion so a test can interrogate it directly.
// TestResolvedGuard_MarshalerCheckSeesBothReceiverForms pins the pointer half
// specifically: deleting it turns that test red with no mutation planted.
// Without it, deleting the half is invisible to the entire committed suite, and
// a reader who assumes "if the type implements it, so does the pointer" — the
// implication holds only in that direction — would simplify the condition and
// see nothing fail.
//
// THE TWO HALVES ARE NOT EQUALLY PROTECTED, AND THE WEAK ONE IS THE VALUE HALF.
// walkMarshalerBan dereferences to a struct kind before calling this, so every
// call from the walk passes a struct type, and for struct T the method set of *T
// contains T's. The POINTER half therefore decides every walked node on its own.
// Measured at 1e022df8: deleting the value half reds exactly one subtest and no
// row of the control table; deleting the pointer half reds eight rows.
//
// The value half is still REQUIRED. It is the half that carries pointer-TYPED
// inputs, which reach this predicate only from callers that do not normalize.
// Deleting it makes the predicate silently wrong for those and that is a false
// negative in the R21 class. It is pinned by exactly one assertion, in the test
// named above; delete the half and that assertion together and the package goes
// green. Anyone tempted to "simplify" this to the pointer disjunct because the
// walk makes it redundant should add a caller-side normalization guarantee
// first, and there is none today.
func implementsJSONMarshaler(typ reflect.Type) bool {
	marshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	return typ.Implements(marshaler) || reflect.PointerTo(typ).Implements(marshaler)
}

// resolvedGuardModulePrefix bounds the recursive marshaller ban. Types outside
// this module are reached but never judged: banning json.Marshaler on someone
// else's type is not a rule we can enforce or a defect we can fix, and time.Time
// and json.RawMessage both implement it legitimately.
const resolvedGuardModulePrefix = "github.com/GoogleCloudPlatform/scion/"

// resolvedGuardMarshalerExceptions lists in-module types permitted to implement
// json.Marshaler despite being reachable from a resolved response.
//
// IT IS EMPTY, AND IT IS DELIBERATELY WRITTEN AS AN EMPTY LIST RATHER THAN
// OMITTED. Measured at the time of writing: zero in-module types reachable from
// the four roots implement json.Marshaler, and zero out-of-module types are
// reachable at all, so nothing needs exempting today. The list exists so that
// the first person who genuinely needs an exception has a declared place to put
// it, with the review that a diff to this list attracts — rather than reaching
// for the easier fix of deleting the recursion.
//
// THE WORD "SITE" BELOW IS A DEFINED TERM, AND THE DEFINITION IS NOT HERE. It is
// stated once, in the failure message inside walkMarshalerBan — search this file
// for "A SITE is". Deliberately not restated at this end: two copies of a
// definition are two definitions and they drift, so the cost is paid as one jump
// rather than as an eventual contradiction.
//
// WHICH USES IT GOVERNS. Every occurrence of the bare noun in this file, with
// three fixed compounds excepted — "call site", "use site" and
// "construction site" — which carry their ordinary programming meanings. Each is
// kept whole on one line here and should stay that way: a compound split across
// a line break reads as a bare noun to a line-based census, which is the only
// instrument anyone has used on this file. That exception is an enumeration and
// not a pattern: "per-site" below IS the defined term, and a new compound is a
// new decision rather than an automatic exemption.
//
// WHY IT IS POINTED AT RATHER THAN LEFT IMPLICIT. A definition placed inside a
// failure message reaches only a reader who has already triggered the assertion;
// whoever edits the map below has not, and exempting a type is the most
// consequential edit this file offers.
//
// KEYED BY TYPE, NOT BY SITE. An entry here exempts EVERY site at which the type
// is reached, including sites the failure message did not name. Read that
// together with the dedup note in walkMarshalerBan: the message names only the
// FIRST path a violating type was reached by, so the reviewer of an exception
// diff is systematically shown a NARROWER blast radius than the exception
// actually has. Measured instance: hubclient.ProjectResourceList is reached as
// both .DefaultResources.Requests and .DefaultResources.Limits, and only
// .Requests is ever named — exempting it on the strength of that message would
// silently exempt .Limits too.
//
// If a per-site exemption is ever genuinely wanted, key this on (type, path) and
// change the walk to match. Do not approximate it by exempting the type.
var resolvedGuardMarshalerExceptions = map[reflect.Type]bool{}

// assertNoCustomMarshaler fails if v, or ANY type reachable from it, implements
// json.Marshaler.
//
// WHY THIS ONE RECURSES WHEN THE OTHER TWO DO NOT. The three interveners are not
// equally dangerous at depth. omitempty, omitzero and a nil embedded pointer are
// SUBTRACTIVE: they can drop a declared key, so the emitted set stays a subset of
// the declared names, and a reader of the struct can still enumerate the worst
// case. A marshaller is the only one that can INVENT a key that appears in no
// field declaration anywhere. That asymmetry is why the ban recurses to unbounded
// depth while assertOnlySafeTagOptions and the exact-set assertion correctly stay
// at the four-type frontier — stretching the tag rule across the boundary would
// fail sixteen legitimate omitempty fields on hubclient.ProjectSettings, where
// absence genuinely means unset.
//
// MEASURED, NOT ASSUMED — this replaced a top-level-only check that was blind to
// four real doors. Planting a pointer-receiver MarshalJSON on hubclient.BucketConfig
// (reached at .Project.Bucket, depth 2) erased "provider" and "name" — both of
// which are NOT omitempty, so mandatory keys set to real values vanished from the
// wire — while every package in ./pkg/... stayed green under BOTH tag modes.
// hubclient.ProjectResourceList at .Project.DefaultResources.Requests is three
// hops out and behaved identically. The surface is depth-unbounded, so the guard
// is too.
//
// NO ADDRESSABILITY MITIGATION EXISTS ON THE CURRENT TYPES, and that is a fact
// about these fields rather than about the class. .Project, .Bucket,
// .DefaultResources, .Requests and .Limits are every one of them POINTER fields,
// and a pointer field is reached through a pointer whatever the parent is, so the
// marshaller fires on json.Marshal(T{}) exactly as it does on json.Marshal(&T{}).
// Add a VALUE-typed struct field tomorrow and the mitigation returns along with
// the divergence it causes: the guard feeding a value would observe a clean
// payload while the handler marshalling a pointer ships the exploited one.
func assertNoCustomMarshaler(t *testing.T, v any) {
	t.Helper()
	walkMarshalerBan(t, v, false)
}

// walkMarshalerBan is assertNoCustomMarshaler with the module-boundary rule made
// switchable, so that the rule itself can be controlled rather than trusted.
//
// judgeOutOfModule is FALSE in production use. It exists because a rule whose
// only stated justification is "insurance against a type nobody has added yet"
// reads as dead code, and the next person deletes it. With it true,
// TestResolvedGuard_ModuleBoundaryRuleIsLoadBearing shows precisely what the
// rule is buying: time.Time implements json.Marshaler legitimately, so without
// the boundary the first standard-library type reached becomes a false positive.
func walkMarshalerBan(t *testing.T, v any, judgeOutOfModule bool) {
	t.Helper()

	// DEDUP GATES REPORTING TOO, AND THE COMMENT THAT USED TO SIT HERE DENIED IT.
	// It claimed "recursion only, never reporting". That is false for struct
	// types: the visited check below returns ABOVE the assertion, so a violating
	// type reachable by several paths is named at the FIRST path only. Measured
	// at 1e022df8 — one marshaller on hubclient.ProjectResourceList, which
	// .Project.DefaultResources reaches as both Requests and Limits:
	//
	//   "DefaultResources.Requests" mentions 2   (one per root that REACHES it)
	//   "DefaultResources.Limits"   mentions 0   <- never named
	//
	// THIS IS NOT A COVERAGE HOLE AND MUST NOT BE READ AS ONE. Implementing
	// json.Marshaler is a property of the TYPE, not of the path it was reached
	// by, so the verdict at the first site is the verdict at every site and no
	// violation can escape. What is lost is blast radius in the diagnostic, which
	// is why the message below says "reached at" and not "reached only at".
	//
	// The soundness argument is worth keeping because it does NOT generalise:
	// per-type dedup is safe here precisely because the judged property is
	// type-determined. It would be unsafe for a path-dependent property — which
	// is one more reason the tag check is not recursed alongside this one.
	//
	// The old comment also cited a case it does not cover: three interface{}
	// fields sharing map[string]interface{}. Interface kinds return at the
	// struct-kind check, which sits BELOW this declaration and ABOVE the visited
	// check that follows it, so they are never reported by this walk at all and
	// never exercised the dedup. The prototype defect that lesson came from was
	// real; generalising it to this code was not.
	//
	// Scope: this map is per-ROOT, rebuilt on each call, so a violating type is
	// named once per root that REACHES it rather than once in total. Reaching is
	// the operative property, not mirror-ness: resolvedResponseTypes lists FOUR
	// roots and the measurement above is 2, because only the two plural
	// ResolvedProjectSettings carry a field that reaches this subgraph. A fifth
	// root that reached it would make the count 3.
	visited := map[reflect.Type]bool{}

	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		for {
			switch typ.Kind() {
			case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
				typ = typ.Elem()
				continue
			}
			break
		}
		if typ.Kind() != reflect.Struct {
			// Interfaces land here. A dynamic type is not knowable statically,
			// so .Project.Runtimes, .Harnesses and .Profiles are a PERMANENT
			// residual of this approach, not an oversight. No reflective guard
			// closes them.
			return
		}
		if pkg := typ.PkgPath(); pkg != "" && !strings.HasPrefix(pkg, resolvedGuardModulePrefix) && !judgeOutOfModule {
			return // out of module: reached, never judged
		}
		if visited[typ] {
			return
		}
		visited[typ] = true

		if !resolvedGuardMarshalerExceptions[typ] {
			assert.Falsef(t, implementsJSONMarshaler(typ),
				"%s implements json.Marshaler, reached at %s.\n"+
					"HOW TO COUNT WHAT YOU ARE LOOKING AT. A SITE is one chain of "+
					"field names descending from one root. The identical chain "+
					"underneath a DIFFERENT root is a SECOND site, not the same site "+
					"printed twice — the term is defined here because the two "+
					"readings give different numbers and the rest of this paragraph "+
					"is unreadable without one of them. The walk emits ONE line for "+
					"every root the type is reachable from, quoting the first site "+
					"that root arrived by, and stays silent about later sites BENEATH "+
					"THAT SAME ROOT. The line count above is therefore a count of "+
					"ROOTS and not of sites. Worked both ways: a type hanging off one "+
					"chain beneath each of two roots emits 2 lines and every site it "+
					"has is quoted; a type hanging off two chains beneath a single "+
					"root emits 1 line and one of its sites is missing. THE OUTPUT "+
					"CANNOT TELL YOU WHICH OF THOSE YOU HAVE. Treat the list as "+
					"possibly short, never as evidence that something else is out "+
					"there, and do not go searching for a chain that may not exist. "+
					"Deleting the marshaller repairs every site simultaneously, "+
					"quoted or not.\n"+
					"A custom marshaller decides at runtime which keys to write, so the "+
					"exact-set assertion — which reads what the encoder emits for one "+
					"constructed value — stops being a bound on what real responses "+
					"contain. This was measured, not theorised: a marshaller that emitted "+
					"\"winner\" only for populated entries put that field on the wire with "+
					"every guard in this file green, and one planted at depth 2 on "+
					"hubclient.BucketConfig erased two NON-omitempty keys with all 54 "+
					"packages green.\n"+
					"If you need a custom marshaller here, this test needs rethinking "+
					"first. If the type is genuinely allowed one, add it to "+
					"resolvedGuardMarshalerExceptions and say why.", typ, path)
		}

		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			// Recurse into anonymous fields even when the embedded TYPE is
			// unexported, and skip NAMED unexported fields. The two are not the
			// same case and the distinction is load-bearing, measured both ways:
			// an embedded unexported struct promotes its exported fields onto the
			// wire, so a marshaller below it fires — {"deep":{"pwned":true}} —
			// while a named unexported field is never marshalled at all, so
			// judging its type would be a false positive on something that cannot
			// reach a response.
			if !f.Anonymous && f.PkgPath != "" {
				continue
			}
			walk(f.Type, path+"."+f.Name)
		}
	}

	rt := reflect.TypeOf(v)
	walk(rt, rt.String())
}

// jsonWireNames returns the JSON object keys encoding/json ACTUALLY emits for
// v. It marshals rather than reflecting, because the wire — not the tag list —
// is the contract, and the two are not the same set.
//
// Reflection over tags cannot see fields promoted from an embedded struct, and
// cannot see a custom MarshalJSON at all. Both are one-line ways to add a field
// to this response, and both were demonstrated against the real type: a field
// added via an embedded unexported struct reached a live 200 response named
// "winner" with the entire suite for this endpoint green.
//
// This helper reports on ONE value. That is only a bound on what real
// responses contain if nothing can suppress a key for the value it is given —
// so callers must pair it with both assertOnlySafeTagOptions and
// assertNoCustomMarshaler. Neither is optional and neither implies the other;
// resolvedResponseTypes carries the coverage argument.
func jsonWireNames(t *testing.T, v any) []string {
	t.Helper()

	b, err := json.Marshal(v)
	require.NoError(t, err, "the response type must marshal; a wire guard cannot "+
		"report on a value the encoder rejects")

	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &m),
		"the response type must marshal to a JSON object, got: %s", b)

	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// (see assertOnlySafeTagOptions below) fails if any field promoted onto v's top-level JSON
// object carries `,omitempty`.
//
// This is what makes jsonWireNames trustworthy on a zero value, and it is the
// second half of the fix — without it the wire guard has a hole the same shape
// as the one it was written to close.
//
// THE ALTERNATIVE WAS TRIED AND MEASURED, AND THE FIRST EXPLANATION OF WHY IT
// FAILED WAS WRONG. The obvious way to beat `omitempty` is to populate the
// value before marshalling, reflectively, so that fields the test does not know
// about are filled too.
//
// The correct statement is about the TRAVERSAL, not about reflect's powers.
// Walking fields by index — Field(i), recursing into embedded types — reaches
// the embedded struct FIELD, whose CanSet is false, so the fill silently skips
// everything promoted through it. Reaching the same data by name is a different
// story: FieldByName("Winner") on an embedded unexported VALUE struct returns
// CanSet=true and SetString works, with no unsafe. Measured:
//
//	Field(i) recursion          -> CanSet=false, skipped
//	FieldByName("Winner")       -> CanSet=true, SetString succeeds
//
// So "reflect cannot set it" is false, and an earlier version of this comment —
// and of the commit message that introduced it — said exactly that. What is
// true is that the by-index fill does not reach it. A by-name fill would.
//
// The fill is still not used here, for the reason in resolvedResponseTypes: it
// cannot address route 2 at all, since FieldByName through a nil embedded
// pointer panics. Prohibiting the shapes is sound; out-constructing them is
// not. Measured on the real type, with the encoder as the judge:
//
//	type ResolvedProjectSetting struct { ...; ctrlSneak }
//	type ctrlSneak struct { Winner string `json:"winner,omitempty"` }
//
//	WIRE:       {"projectSet":true,"projectValue":null,"hubDefault":"","winner":"sneaked"}
//	GUARD SAW:  [hubDefault projectSet projectValue]
//
// — with every guard in this file green. A same-package handler can assign to
// the promoted field, so that is a live route and not a curiosity. A reflective
// fill would have been a mechanism that looks like it closes the hole while not
// closing it, which is the same failure as a comment that explains why an
// unsound skip is safe.
//
// Banning `omitempty` outright is the honest version of that half, and it costs
// nothing here: this response never omits keys by design. Every registered
// setting appears every time, and an unset projectValue is explicit `null`
// rather than a missing key, precisely so a client cannot read "absent" out of
// silence. `omitempty` on these types would contradict the documented contract
// before it ever troubled a test.
//
// HALF is the operative word. This closes the tag route and nothing else; a
// custom marshaller suppresses keys without any tag being involved, and this
// function is blind to it. assertNoCustomMarshaler closes that one. See
// resolvedResponseTypes for why both are needed and neither implies the other.
// safeJSONTagOptions is an ALLOWLIST: json tag options known not to affect
// WHICH KEYS the encoder emits. Anything not listed here is rejected.
//
// This is an allowlist rather than a denylist of the bad options, and that was a
// deliberate reversal after being caught. The check named `omitempty` and only
// `omitempty`; Go 1.24 added `omitzero`, a second spelling of the same hazard,
// and this file did not mention it. Measured on the real types at 25f82c33 with
// `winner,omitzero` planted on BOTH hub and hubclient ResolvedProjectSetting:
// the whole suite passed in both tag modes while the key reached the wire —
// byte-for-byte the defect the coverage argument above says this guard exists to
// stop, with one word changed.
//
// The two designs fail in opposite directions, and only one of them is safe:
//
//   - denylist: an option nobody here has heard of is ACCEPTED SILENTLY. That is
//     the defect itself, and it is how omitzero survived.
//   - allowlist: an option nobody here has heard of FAILS LOUDLY, and someone
//     spends one line and a sentence of justification adding it.
//
// The cost is real — a legitimate future option produces a red that has to be
// cleared by hand — and it is the right trade only because this check is scoped
// to four enumerated types that are meant to be boring data. It would be
// obnoxious applied broadly.
//
// IT IS DELIBERATELY EMPTY. The rule is therefore "a json tag on these types
// carries a NAME AND NOTHING ELSE"; any comma is rejected. Measured across all
// four types: ten fields, zero tag options, zero commas — so the strict form
// costs nothing today, and no exemption needs carving for it now.
//
// `string` is the obvious candidate if one is ever needed (it changes how a
// value is ENCODED, a number written as a JSON string, never whether its key
// appears). It is deliberately NOT pre-authorised: an allowlist entry nobody
// uses is an unreviewed exemption sitting open.
//
// THREE BOUNDARY CASES a reader will reach for, none of which is a hole:
//
//   - json:"-"   no comma, so this check ACCEPTS it. But the field then leaves
//     the wire entirely, so its key is missing from the zero marshal and the
//     exact-set assertion fails against expectedResolved*Fields. Caught
//     downstream, not here.
//   - json:"-,"  a literal "-" key. HAS a comma, so it is rejected right here.
//   - no tag     no comma, ACCEPTED. The key becomes the Go field name, present
//     in the zero marshal but absent from the expected set, so the exact-set
//     assertion fails. Caught downstream.
//
// The comma rule and the exact-set assertion cover the three between them. This
// is spelled out because `json:"-"` sailing through looks like a hole, and a
// reader who "fixes" it here will be hardening the wrong check.
var safeJSONTagOptions = map[string]bool{}

// assertOnlySafeTagOptions fails if any field whose key is promoted into the
// resolved-settings object carries a json tag option that is not on
// safeJSONTagOptions.
func assertOnlySafeTagOptions(t *testing.T, v any) {
	t.Helper()

	typ := reflect.TypeOf(v)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	require.Equal(t, reflect.Struct, typ.Kind())

	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag := field.Tag.Get("json")
			if tag == "-" {
				continue
			}
			parts := strings.Split(tag, ",")

			// An embedded struct with no tag NAME has its fields promoted into
			// this object, so its tags are this object's tags. reflect can read
			// the TYPE of an unexported embedded field regardless of whether it
			// could set a value there, which is what lets this check reach
			// fields a by-index fill skips.
			//
			// Deliberately NOT dereferencing pointers. An earlier version did,
			// in order to walk through them — which meant it accepted an
			// embedded POINTER, the one shape whose promoted keys vanish
			// wholesale from a zero value. That shape is rejected outright by
			// assertNoEmbeddedStructPointer; dereferencing here would inspect
			// its tags and pronounce it clean.
			if field.Anonymous && parts[0] == "" && field.Type.Kind() == reflect.Struct {
				walk(field.Type, path+field.Name+".")
				continue
			}

			for _, opt := range parts[1:] {
				if safeJSONTagOptions[opt] {
					continue
				}
				assert.Failf(t, "unsafe json tag option",
					"%s%s carries the json tag option `%s`, which is not on the "+
						"known-safe list for the resolved-settings response types, "+
						"which carry a tag NAME and nothing else.\n"+
						"Two reasons, and the second is the one that bites:\n"+
						"  1. This response never omits keys. Every registered setting is "+
						"reported every time, and an unset projectValue is explicit null, "+
						"so that a client cannot infer \"absent\" from a missing key.\n"+
						"  2. An option that suppresses a zero-valued field hides it from "+
						"the encoder, and the shape guard reads the encoder's output. Such "+
						"a field can reach real clients while "+
						"TestResolvedSettingsResponseShape_NoEffectiveValue stays green. "+
						"This is not hypothetical: `omitempty` and `omitzero` both do it, "+
						"and `omitzero` additionally honours a user-defined IsZero(), so "+
						"the key set can depend on a method's opinion of zero-ness rather "+
						"than on the value being zero at all.\n"+
						"If `%s` genuinely cannot change which keys are emitted, add it to "+
						"safeJSONTagOptions with a note saying why. Do not delete this "+
						"check to make it pass.",
					path, field.Name, opt, opt)
			}
		}
	}
	walk(typ, "")
}

// jsonTagNames returns the JSON field names declared by a struct type's tags:
// the tag name is used, options such as ",omitempty" are stripped, fields
// tagged "-" are excluded, and an untagged field contributes its Go name.
//
// This is NOT a model of the wire and must not be used as one — see
// jsonWireNames, which is. It survives here for the hub↔hubclient comparison,
// where the question is whether the two types DECLARE the same names. That is a
// narrower property than wire equality and it is worth asserting separately:
// a MarshalJSON on one side could make the wires agree while the declarations
// diverge, which is a trap for anyone reading the types to learn the contract.
func jsonTagNames(t *testing.T, v any) []string {
	t.Helper()

	typ := reflect.TypeOf(v)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	require.Equalf(t, reflect.Struct, typ.Kind(),
		"jsonTagNames expects a struct, got %s", typ.Kind())

	var names []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
			// Unexported by Go's rules. NOTE, because the obvious comment here
			// is false: this does NOT mean "never marshalled". encoding/json
			// promotes the exported fields of an embedded UNEXPORTED struct
			// type onto the wire, "since they may have exported fields", and
			// that field carries a non-empty PkgPath. Skipping it here is a
			// property of this reflective helper, not of the encoder — which
			// is exactly why the exact-set guard runs on jsonWireNames instead.
			continue
		}
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			name = field.Name
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestResolvedSettings_RegistryNoDrift is the bidirectional coverage guard
// between the authoritative registry and the resolved endpoint's descriptor
// table.
//
// Why a second structure exists at all, and why this test is the thing that
// makes it safe: if the response were built by ranging over projectSettingKeys
// alone, every registry key would appear in the response by construction, this
// guard could never fail, and its passing would mean nothing. A check that
// cannot fail is worse than no check because it is read as protection. The
// descriptor table holds the per-key wiring that the key string cannot supply,
// and this test asserts the two cover each other EXACTLY.
//
// Both directions matter:
//
//   - registry -> descriptor: a new setting that nobody wired reaches the
//     response as "unknown" forever, silently under-reporting.
//   - descriptor -> registry: a descriptor for a key that is no longer a
//     project setting means the endpoint is prepared to report something that
//     is not part of the contract.
func TestResolvedSettings_RegistryNoDrift(t *testing.T) {
	require.NotEmpty(t, projectSettingKeys,
		"the registry is empty; this guard would pass vacuously")
	require.NotEmpty(t, resolvedSettingDescriptors,
		"the descriptor table is empty; this guard would pass vacuously")

	// Direction 1: every registered setting is wired into the endpoint.
	for _, key := range projectSettingKeys {
		assert.Containsf(t, resolvedSettingDescriptors, key,
			"project setting %q is in projectSettingKeys but has no entry in "+
				"resolvedSettingDescriptors, so GET /settings/resolved reports it as "+
				"\"unknown\" forever instead of answering for it. Add a descriptor in "+
				"project_settings_resolved.go naming the hub source for this key "+
				"(hubSourceNone if there is genuinely no hub-level counterpart).",
			key)
	}

	// Direction 2: nothing is wired that is not a registered setting.
	registered := make(map[string]bool, len(projectSettingKeys))
	for _, key := range projectSettingKeys {
		registered[key] = true
	}
	for key := range resolvedSettingDescriptors {
		assert.Containsf(t, registered, key,
			"resolvedSettingDescriptors has an entry for %q, which is not in "+
				"projectSettingKeys. RULE OUT A MISSPELLED KEY FIRST: the setting may "+
				"still be registered under its correct spelling, in which case the fix is "+
				"to correct this key and nothing needs deleting or adding. That case also "+
				"reddens the OTHER direction above, whose advice — add a descriptor for "+
				"the seemingly unwired key — would leave two descriptors for one setting "+
				"and then trip the count assertion below. Failing that: the key was "+
				"removed from the registry and this descriptor should go with it, or it "+
				"was never a project setting and the endpoint must not report it. Those "+
				"are the causes seen so far and the list is not closed; work out which "+
				"one you have before editing, because the fixes are mutually exclusive.",
			key)
	}

	assert.Equalf(t, len(projectSettingKeys), len(resolvedSettingDescriptors),
		"registry has %d keys but the descriptor table has %d entries",
		len(projectSettingKeys), len(resolvedSettingDescriptors))
}

// TestResolvedSettings_DescriptorsWellFormed checks the descriptor table's
// internal consistency, so that a malformed entry fails here rather than
// degrading a live response to "unknown".
func TestResolvedSettings_DescriptorsWellFormed(t *testing.T) {
	for key, desc := range resolvedSettingDescriptors {
		switch desc.source {
		case hubSourceAgentDefaults:
			assert.NotEmptyf(t, desc.path,
				"descriptor %q reads agent_defaults but has no path, so it can never "+
					"report anything but unknown", key)
		case hubSourceNone, hubSourceTelemetryDefault:
			assert.Emptyf(t, desc.path,
				"descriptor %q does not read agent_defaults but carries a path %v, "+
					"which is never used and is therefore misleading", key, desc.path)
			assert.Falsef(t, desc.absentWhenMissing,
				"descriptor %q sets absentWhenMissing, which only applies to "+
					"agent_defaults lookups", key)
		default:
			t.Errorf("descriptor %q has unknown source %d", key, desc.source)
		}
	}
}

// TestResolvedSettings_ResponseCoversRegistry is the deliverable the phase asks
// for in its most direct form: every registered project setting appears as a
// key in an actual response body.
//
// This is deliberately asserted against a real constructed response rather than
// against the descriptor table, so that a bug in the builder — an early
// continue, a filtered key — is caught even when the tables agree with each
// other.
func TestResolvedSettings_ResponseCoversRegistry(t *testing.T) {
	project := &store.Project{
		ID:          "p-1",
		Annotations: map[string]string{},
	}

	// A zero Server has no OperationalSettings and no config, which is the
	// file/SQLite path: hub presence is unknowable and must be reported as
	// such rather than as absent.
	resp := (&Server{}).resolvedProjectSettings(project)
	require.NotNil(t, resp)

	for _, key := range projectSettingKeys {
		assert.Containsf(t, resp.Settings, key,
			"registered project setting %q is missing from the resolved response. "+
				"The response must report every key in projectSettingKeys.", key)
	}
	assert.Lenf(t, resp.Settings, len(projectSettingKeys),
		"the resolved response has %d keys but the registry has %d",
		len(resp.Settings), len(projectSettingKeys))
}

// TestResolvedSettingsResponseShape_NoEffectiveValue is the negative drift
// guard: it fails if the resolved response grows ANY new field.
//
// It is an exact-set assertion, not a denylist. A denylist of forbidden names
// ("effectiveValue", "resolvedValue", ...) would be a hypothesis about what a
// future contributor will CALL the field, and the entire problem is that this
// cannot be predicted — "value", "winner", "current" or "appliedValue" would
// all sail through. Asserting the set exactly means any addition fails and the
// author has to come and argue for it.
//
// THAT ARGUMENT IS ONLY TRUE OF THE VERSION BELOW. The first version of this
// guard reflected over struct tags, and review demonstrated that a field added
// through an embedded unexported struct reached a live response named "winner"
// — the very name this comment offers as one a denylist would miss — with the
// whole suite green. For the whole of the phase in which this comment claimed
// otherwise, the denylist in project_settings_resolved_test.go was the only
// check that could fire, and it is what fired. The exact-set assertion is
// better than a denylist only once it reads the encoder's output, which is why
// it now runs on jsonWireNames and why both checks are kept.
//
// It asserts on the marshalled wire shape rather than on Go field names because
// the wire is the contract: renaming a Go field while keeping its tag must not
// trip this, and changing a tag must.
//
// KNOWN AND DELIBERATE LIMIT OF THIS GUARD: it covers the resolved wrapper and
// the per-key object. It does NOT look inside hubclient.ProjectSettings, which
// the "project" key carries whole, NOR into the graph reachable from it, which
// runs to depth 3 and beyond.
//
// Two ways that graph can change the wire, not one. A field ADDED to a reachable
// type reaches this response unguarded. A custom MarshalJSON on a reachable type
// also REMOVES keys whose values were set — measured, with telemetryEnabled set
// and then absent from the bytes. State the removal case too: a limit statement
// that understates the limit is worse than none, because it is what the next
// reader relies on instead of looking.
//
// Exact-setting hubclient.ProjectSettings here was considered and rejected: it
// has sixteen fields that legitimately change for reasons unrelated to this
// endpoint, so that guard would fail on other people's honest work, and a guard
// that cries wolf gets deleted.
//
// THAT ARGUMENT IS ABOUT EXACT-SETTING ONLY. It does not carry to the recursive
// marshaller ban in assertNoCustomMarshaler, and must not be read as having
// considered and rejected it — it was not considered when this comment was
// written. A marshaller ban cannot cry wolf here: zero reachable types implement
// json.Marshaler today, so it has zero false positives, measured, with a
// positive control. That ban is what covers the removal case; this test does not.
//
// The realistic version of the ADD drift — the "project" field being retyped to a
// locally widened struct — is covered by TestResolvedSettings_ProjectFieldTypeIdentity
// below instead.
func TestResolvedSettingsResponseShape_NoEffectiveValue(t *testing.T) {
	const rationale = "\n\n" +
		"resolved-settings response shape changed.\n" +
		"This test exists to prevent well-intentioned additions. If you are adding an effective\n" +
		"value field, read the /settings/resolved design rationale first: this endpoint\n" +
		"deliberately does NOT resolve precedence, because we do not own the ordering. See the\n" +
		"header comment in pkg/hub/project_settings_resolved.go, the doc comment on\n" +
		"hubclient.ResolvedProjectSetting, and docs-site reference/project-settings-resolved.md.\n" +
		"Note this applies to a hub VALUE too, not only to an 'effective' one: {projectValue,\n" +
		"hubValue} side by side asserts that nothing sits between them, which is the same\n" +
		"precedence claim by another route.\n" +
		"If you are adding an unrelated field, add its JSON name to the expected set below."

	// The expected sets are package-level (expectedResolvedWrapperFields,
	// expectedResolvedEntryFields). A legitimate new field is added THERE, in
	// one obvious place — the guard is meant to be a speed bump that forces an
	// argument, not a wall that gets deleted by the first person it
	// inconveniences.
	// Step 1 is what makes step 2 sound. These are not three good ideas: they
	// are an enumeration of every way a field can exist on these types and
	// still be missing from a marshalled zero value, and step 2 is complete
	// only because the enumeration is. The list was wrong at two until review
	// found the third — see the coverage argument on resolvedResponseTypes,
	// which records what each one is and how it was measured.
	for _, v := range resolvedResponseTypes() {
		assertResponseKeySetIsTypeDetermined(t, v)
	}

	// Step 2: the encoder's actual output is exactly the expected set.
	//
	// A ZERO value is sufficient here, and that is a conclusion rather than a
	// convenience. encoding/json emits a key for every exported field it knows
	// about; the only two things that can suppress one are `omitempty` (step
	// 1a) and a custom marshaller choosing not to write it (step 1b). With both
	// prohibited, what the encoder emits for the zero value IS the complete key
	// set.
	assert.Equal(t, expectedResolvedWrapperFields,
		jsonWireNames(t, ResolvedProjectSettings{}),
		"hub.ResolvedProjectSettings"+rationale)
	assert.Equal(t, expectedResolvedEntryFields,
		jsonWireNames(t, ResolvedProjectSetting{}),
		"hub.ResolvedProjectSetting"+rationale)

	// The SAME assertion over a POINTER, because that is the production path:
	// resolvedProjectSettings returns *ResolvedProjectSettings and writeJSON
	// marshals that pointer. The value form above is what this guard has always
	// read, and the two can disagree — encoding/json invokes a pointer-receiver
	// MarshalJSON only on an addressable value, so a marshaller reachable from
	// &T but not from T would rewrite the response with the value assertion
	// green.
	//
	// assertNoCustomMarshaler currently makes that divergence impossible, so
	// today these two pairs are provably identical and this looks redundant. It
	// is kept because the PAIR is itself a second, independent control on the
	// marshaller ban, reached by a different route than the instrument table: if
	// the ban is ever weakened AND a pointer-receiver marshaller appears, the &T
	// assertion fires and the T assertion does not. A proof resting on the ban
	// does not survive the ban being narrowed; this does.
	assert.Equal(t, expectedResolvedWrapperFields,
		jsonWireNames(t, &ResolvedProjectSettings{}),
		"*hub.ResolvedProjectSettings (the production path)"+rationale)
	assert.Equal(t, expectedResolvedEntryFields,
		jsonWireNames(t, &ResolvedProjectSetting{}),
		"*hub.ResolvedProjectSetting (the production path)"+rationale)

	// The hubclient mirror is part of the same contract: a field added to only
	// one side would produce a client type that silently disagrees with the
	// server.
	//
	// These two carry a DIFFERENT message from the hub-side pair above, and the
	// difference is the point. A contributor who adds a field reaches this
	// assertion having already done what the shared text tells them to do, so
	// repeating "add its JSON name to the expected set" reads as the escape
	// hatch being broken. What is actually missing at this point is the mirror
	// type, in another package, which nothing has yet named.
	const mirrorHint = "\n\n" +
		"You have updated the hub-side type and the expected set, and this assertion is\n" +
		"still failing. That is not a bug in the hatch: the field also has to be added to\n" +
		"the client mirror in pkg/hubclient/types.go (ResolvedProjectSettings /\n" +
		"ResolvedProjectSetting). The two types are hand-mirrored across a package\n" +
		"boundary. This is NOT the only assertion that notices when they drift, and\n" +
		"the list below may not be closed: the declared-tag equality a few lines down\n" +
		"fires as well, and so does the clean-substitute control row in\n" +
		"project_settings_resolved_wireup_test.go — which reports its OWN machinery as\n" +
		"broken rather than the mirror, so do not start debugging there.\n" +
		"What this assertion contributes that the tag equality cannot is the hardcoded\n" +
		"expected set: it reads the mirror's wire names and compares them to a fixed\n" +
		"list, so it fires even when both sides were changed together and agree with\n" +
		"each other. The tag equality compares the two sides only to one another and\n" +
		"is silent in that case. Both directions measured at the time of writing."

	assert.Equal(t, expectedResolvedWrapperFields,
		jsonWireNames(t, hubclient.ResolvedProjectSettings{}),
		"hubclient.ResolvedProjectSettings"+rationale+mirrorHint)
	assert.Equal(t, expectedResolvedEntryFields,
		jsonWireNames(t, hubclient.ResolvedProjectSetting{}),
		"hubclient.ResolvedProjectSetting"+rationale+mirrorHint)

	// Declared-tag equality between the two sides, which is a different
	// question from wire equality: a MarshalJSON on one side could reconcile
	// the wires while leaving the declarations divergent, and the declarations
	// are what the next person reads to learn the contract.
	assert.Equal(t,
		jsonTagNames(t, ResolvedProjectSettings{}),
		jsonTagNames(t, hubclient.ResolvedProjectSettings{}),
		"hub and hubclient ResolvedProjectSettings declare different JSON tags"+rationale)
	assert.Equal(t,
		jsonTagNames(t, ResolvedProjectSetting{}),
		jsonTagNames(t, hubclient.ResolvedProjectSetting{}),
		"hub and hubclient ResolvedProjectSetting declare different JSON tags"+rationale)
}

// TestResolvedSettings_MirrorAcceptsServerOutput round-trips a server-shaped
// response through the client mirror.
//
// Everything else that checks the hub↔hubclient relationship compares NAMES.
// Names are not the contract a client depends on: retyping the mirror's
// HubDefault from ResolvedHubDefault to bool, or ProjectValue from *string to
// string, keeps every tag identical and breaks every client at runtime. This
// test is the only thing that makes a real server payload prove it can be
// decoded by the type we ship to clients.
//
// It carries no build tag, and that is load-bearing rather than incidental. CI
// runs -tags no_sqlite, under which project_settings_resolved_test.go does not
// compile in at all — so this is the only place mirror type drift can be caught
// where enforcement actually happens. It deliberately uses no store, no server
// and no HTTP, so nothing can ever make it need one.
func TestResolvedSettings_MirrorAcceptsServerOutput(t *testing.T) {
	value := "sonnet"
	server := ResolvedProjectSettings{
		Project: &hubclient.ProjectSettings{DefaultModel: value},
		Settings: map[string]ResolvedProjectSetting{
			projectSettingDefaultModel: {
				ProjectSet:   true,
				ProjectValue: &value,
				HubDefault:   ResolvedHubDefaultUnknown,
			},
		},
	}

	b, err := json.Marshal(server)
	require.NoError(t, err)

	var mirror hubclient.ResolvedProjectSettings
	require.NoError(t, json.Unmarshal(b, &mirror),
		"a real server response must unmarshal into the hubclient mirror; the "+
			"JSON-tag equality assertion above checks names only and cannot see "+
			"a type change. Payload: %s", b)

	entry, ok := mirror.Settings[projectSettingDefaultModel]
	require.Truef(t, ok, "the mirror lost the %q entry", projectSettingDefaultModel)

	assert.Equal(t, hubclient.ResolvedHubDefaultUnknown, entry.HubDefault,
		"the tri-state must survive the round trip as a tri-state. If this is now "+
			"a bool, \"unknown\" has become \"false\" and the client is being told "+
			"the hub has no default when nobody looked.")
	assert.True(t, entry.ProjectSet)
	require.NotNil(t, entry.ProjectValue,
		"projectValue must stay a pointer: null and \"\" are different answers")
	assert.Equal(t, value, *entry.ProjectValue)

	require.NotNil(t, mirror.Project, "the mirror dropped the \"project\" sub-object")
	assert.Equal(t, value, mirror.Project.DefaultModel)
}

// TestResolvedSettings_AbsentWhenMissingMatchesSchema pins every
// absentWhenMissing flag to settings-v1.schema.json, which the descriptor
// table's own doc comment names as its source.
//
// Before this test, that source was a hand copy: sixteen booleans transcribed
// from a schema nothing compared them against. Drift in one direction is
// merely over-cautious (UNKNOWN where ABSENT was available). Drift in the
// other direction makes the endpoint state a positive falsehood — "the hub has
// no default for this" when nobody could actually tell — with the whole suite
// green, because the only other test of these semantics hardcodes its expected
// column to mirror the descriptor it is testing and therefore cannot dissent.
//
// WHAT THIS TEST DOES AND DOES NOT SETTLE. It removes sixteen hand-copied
// VALUES and introduces one hand-written RULE:
//
//	absentWhenMissing is true if and only if the schema makes the Go zero
//	value unreachable for that field.
//
// That rule is a human judgement, not something the schema states. Its
// justification is the write path: AgentDefaultsSettings is marshalled with
// `omitempty`, so an explicitly-zero value is discarded before it is persisted
// and reads back identically to "never set". ABSENT is therefore honest only
// where the zero value could not have been persisted in the first place — which
// is what a `minimum`/`minLength` constraint decides. One rule that review can
// check beats sixteen copies that it cannot, but the rule is the part to argue
// with if you disagree; the schema will not settle it for you.
func TestResolvedSettings_AbsentWhenMissingMatchesSchema(t *testing.T) {
	schema := loadSettingsSchema(t)

	checked := 0
	for key, desc := range resolvedSettingDescriptors {
		if desc.source != hubSourceAgentDefaults {
			// No schema node to compare against. DescriptorsWellFormed already
			// asserts these carry absentWhenMissing == false.
			continue
		}
		require.NotEmptyf(t, desc.path, "descriptor %q reads agent_defaults with no path", key)

		node := resolveSchemaPath(t, schema, desc.path)
		zeroUnpersistable := schemaExcludesZeroValue(node)

		assert.Equalf(t, zeroUnpersistable, desc.absentWhenMissing,
			"descriptor %q (schema path %v) says absentWhenMissing=%v, but the schema "+
				"makes its zero value %s.\n"+
				"These must agree. The write path drops an explicit zero via omitempty, so "+
				"reporting ABSENT is honest ONLY where the schema makes the zero value "+
				"unreachable; otherwise \"configured empty\" and \"never configured\" are the "+
				"same bytes and the only honest answer is UNKNOWN.\n"+
				"If you just changed the schema, change this descriptor to match. If you "+
				"believe the rule itself is wrong, argue with the doc comment on this test — "+
				"do not just flip the boolean.",
			key, desc.path, desc.absentWhenMissing,
			map[bool]string{
				true:  "unreachable (a constraint excludes it)",
				false: "reachable, and then dropped by omitempty",
			}[zeroUnpersistable])
		checked++
	}

	// Without this the loop could pass by checking nothing at all — the exact
	// class of unfalsifiable guard the descriptor table exists to avoid.
	assert.GreaterOrEqual(t, checked, 10,
		"expected at least the ten agent_defaults-backed descriptors to be checked "+
			"against the schema, got %d; if the table shrank legitimately, lower this "+
			"floor deliberately rather than letting the guard go quiet", checked)
}

// loadSettingsSchema reads the settings schema from disk. The path is relative
// to this package's directory, which is where `go test` runs.
func loadSettingsSchema(t *testing.T) map[string]any {
	t.Helper()

	const path = "../config/schemas/settings-v1.schema.json"
	raw, err := os.ReadFile(path)
	require.NoErrorf(t, err, "cannot read %s. This test pins the descriptor table to the "+
		"schema; if the schema moved, repoint it rather than deleting the test.", path)

	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))
	return schema
}

// resolveSchemaPath walks a descriptor path through the schema's "properties"
// maps, resolving "$ref" at each step, and fails the test if any step is
// missing.
//
// Failing loudly is the point. The five default_resources leaves sit behind
//
//	"default_resources": {"$ref": "#/$defs/resourceSpec", "description": ...}
//
// so a walk that reads "properties" without resolving the ref finds nothing —
// and a version of this helper that returned an empty node instead of failing
// would derive absentWhenMissing for five keys from a node containing no
// constraints whatsoever. It would report agreement it had never tested, which
// is the very failure this test was written to fix, reappearing inside the fix.
//
// Path length is not special-cased. Today's paths are one, two and three
// segments long (default_resources/disk is the two), and hardcoding the set is
// how the next nesting change gets silently skipped.
func resolveSchemaPath(t *testing.T, schema map[string]any, path []string) map[string]any {
	t.Helper()

	node := resolveSchemaRef(t, schema, schema)
	for i, segment := range path {
		props, ok := node["properties"].(map[string]any)
		require.Truef(t, ok,
			"schema path %v: no \"properties\" at segment %d (%q). The descriptor claims a "+
				"leaf the schema does not describe.", path, i, segment)

		child, ok := props[segment].(map[string]any)
		require.Truef(t, ok,
			"schema path %v: %q is not present under properties at segment %d. Either the "+
				"schema dropped it or the descriptor's path is wrong; both are bugs.",
			path, segment, i)

		node = resolveSchemaRef(t, schema, child)
	}
	return node
}

// resolveSchemaRef replaces a "$ref" with its target, preserving sibling keys.
//
// Siblings are kept rather than discarded because a constraint written beside a
// $ref applies in addition to the referenced schema, and dropping it would make
// this test under-report exactly the constraints it is here to read.
func resolveSchemaRef(t *testing.T, root, node map[string]any) map[string]any {
	t.Helper()

	ref, ok := node["$ref"].(string)
	if !ok {
		return node
	}

	const prefix = "#/$defs/"
	require.Truef(t, strings.HasPrefix(ref, prefix),
		"unsupported $ref %q: this helper resolves only local %s refs. Teach it the new "+
			"form rather than letting the lookup fall through to an empty node.", ref, prefix)

	defs, ok := root["$defs"].(map[string]any)
	require.True(t, ok, "schema has no $defs but a $ref points into it")
	target, ok := defs[strings.TrimPrefix(ref, prefix)].(map[string]any)
	require.Truef(t, ok, "$ref %q does not resolve", ref)

	merged := make(map[string]any, len(target)+len(node))
	for k, v := range target {
		merged[k] = v
	}
	for k, v := range node {
		if k != "$ref" {
			merged[k] = v
		}
	}
	return merged
}

// schemaExcludesZeroValue reports whether the schema forbids the Go zero value
// for a leaf: 0 for an integer, "" for a string.
//
// This is the single hand-written rule described on
// TestResolvedSettings_AbsentWhenMissingMatchesSchema. It is deliberately
// narrow — only `minimum` and `minLength` are consulted — because those are the
// constructs actually in use. A future `enum` or `pattern` that excludes the
// zero value would be read here as "does not exclude", which errs toward
// UNKNOWN: the over-cautious direction, never the false-statement direction.
func schemaExcludesZeroValue(node map[string]any) bool {
	if min, ok := node["minimum"].(float64); ok && min > 0 {
		return true
	}
	if minLen, ok := node["minLength"].(float64); ok && minLen > 0 {
		return true
	}
	return false
}

// TestResolvedSettings_ProjectFieldTypeIdentity closes the realistic half of
// the blind spot named above.
//
// The guard above cannot police hubclient.ProjectSettings' own field list
// without failing on unrelated honest changes. What it CAN police cheaply is
// that the "project" key still carries exactly that shared type, rather than a
// locally widened struct that embeds it and adds a computed or effective-value
// field alongside. That is the drift this endpoint is actually at risk of, and
// unlike a field addition upstream it is always a deliberate act by someone
// working on this endpoint.
func TestResolvedSettings_ProjectFieldTypeIdentity(t *testing.T) {
	field, ok := reflect.TypeOf(ResolvedProjectSettings{}).FieldByName("Project")
	require.True(t, ok, "ResolvedProjectSettings has no Project field")

	assert.Equal(t, reflect.TypeOf(&hubclient.ProjectSettings{}), field.Type,
		"the resolved response's \"project\" field must remain exactly "+
			"*hubclient.ProjectSettings. If it has been retyped to a local struct, the "+
			"exact-set shape guard no longer sees what this endpoint actually returns — "+
			"a widened wrapper is the supported way to sneak an effective value into this "+
			"response, which is precisely what these guards exist to prevent.")
}

// marshalerCtrlValue carries a VALUE-receiver MarshalJSON. Both receiver forms
// of this type satisfy json.Marshaler, so either half of the check catches it.
type marshalerCtrlValue struct {
	X int `json:"x"`
}

func (marshalerCtrlValue) MarshalJSON() ([]byte, error) { return []byte(`{"custom":1}`), nil }

// marshalerCtrlPointer carries a POINTER-receiver MarshalJSON. Only *T satisfies
// json.Marshaler; T does not. This is the shape a value-receiver-only check misses.
type marshalerCtrlPointer struct {
	X int `json:"x"`
}

func (*marshalerCtrlPointer) MarshalJSON() ([]byte, error) { return []byte(`{"custom":1}`), nil }

// marshalerCtrlNone is the negative control: no custom marshaller at all.
type marshalerCtrlNone struct {
	X int `json:"x"`
}

// TestResolvedGuard_MarshalerCheckSeesBothReceiverForms tests the INSTRUMENT, not
// the response types.
//
// The marshaller ban is the only thing standing between this guard and a defect
// that was found in review, and its pointer half is solely responsible for
// catching the pointer-receiver form. Measured: with a pointer-receiver
// MarshalJSON planted on the real ResolvedProjectSettings, the full condition
// fails and the value half alone passes. Nothing in the committed suite noticed
// the difference, because a planted mutation is not a test — so this is that
// test.
//
// It asserts the halves SEPARATELY rather than just asserting the combined
// result. A control that only checked the combined value would stay green if
// someone deleted the pointer half and the value half happened to cover the
// cases, which is the same unattributed-red mistake this file's own controls
// made earlier: red, but not for the reason claimed.
func TestResolvedGuard_MarshalerCheckSeesBothReceiverForms(t *testing.T) {
	marshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()

	t.Run("pointer receiver is invisible to the value half", func(t *testing.T) {
		typ := reflect.TypeOf(marshalerCtrlPointer{})

		// The attribution. If this pair ever flips, the comment on
		// implementsJSONMarshaler is wrong and the pointer half is redundant.
		assert.False(t, typ.Implements(marshaler),
			"a pointer-receiver MarshalJSON must NOT satisfy json.Marshaler on the "+
				"value type; if it does, this control no longer distinguishes the halves")
		assert.True(t, reflect.PointerTo(typ).Implements(marshaler),
			"a pointer-receiver MarshalJSON must satisfy json.Marshaler on the pointer type")

		assert.True(t, implementsJSONMarshaler(typ),
			"the check must reject a pointer-receiver marshaller. If this fails, the "+
				"`|| reflect.PointerTo(...)` half has been removed, and the guard now "+
				"permits exactly the receiver form the handler uses")
	})

	t.Run("value receiver is caught too", func(t *testing.T) {
		typ := reflect.TypeOf(marshalerCtrlValue{})

		assert.True(t, typ.Implements(marshaler))
		assert.True(t, reflect.PointerTo(typ).Implements(marshaler),
			"a value-receiver method is in the pointer type's method set as well; this "+
				"is why deleting the VALUE half alone would not turn these cases red, "+
				"and why the two halves are not symmetric")
		assert.True(t, implementsJSONMarshaler(typ))
	})

	// Pins the VALUE half. It is unreachable from today's call sites, which all
	// pass values — deleting it leaves every other case in this test green — so
	// without this subtest the half is undefended and reads as redundant. It is
	// not: for a pointer TYPE the pointer half goes false, because PointerTo(*T)
	// is **T with an empty method set.
	t.Run("pointer type is invisible to the pointer half", func(t *testing.T) {
		typ := reflect.TypeOf(&marshalerCtrlPointer{})

		assert.True(t, typ.Implements(marshaler),
			"*T satisfies json.Marshaler directly")
		assert.False(t, reflect.PointerTo(typ).Implements(marshaler),
			"PointerTo(*T) is **T and satisfies nothing; if this ever flips, the "+
				"disjointness argument on implementsJSONMarshaler is wrong")

		assert.True(t, implementsJSONMarshaler(typ),
			"the check must reject a marshaller reached through a pointer TYPE. If "+
				"this fails, the `typ.Implements(marshaler)` half has been removed as "+
				"redundant — it is not redundant, it is merely unused today")
	})

	t.Run("no marshaller is not rejected", func(t *testing.T) {
		typ := reflect.TypeOf(marshalerCtrlNone{})

		assert.False(t, implementsJSONMarshaler(typ),
			"the check must not fire on an ordinary struct; without this the ban could "+
				"pass by rejecting everything")
	})

	// The reason the receiver form matters at all: encoding/json calls a
	// pointer-receiver marshaller only on an addressable value. The guard marshals
	// a value, production marshals a pointer, so the two disagree about what this
	// type even emits.
	t.Run("encoding/json honours the receiver form", func(t *testing.T) {
		asValue, err := json.Marshal(marshalerCtrlPointer{X: 7})
		require.NoError(t, err)
		asPointer, err := json.Marshal(&marshalerCtrlPointer{X: 7})
		require.NoError(t, err)

		assert.JSONEq(t, `{"x":7}`, string(asValue),
			"marshalling the VALUE must bypass the pointer-receiver marshaller")
		assert.JSONEq(t, `{"custom":1}`, string(asPointer),
			"marshalling the POINTER must invoke it — this divergence is why the ban "+
				"cannot look at one receiver form only")
	})
}

// TestResolvedGuard_ModuleBoundaryRuleIsLoadBearing pins the module-boundary
// rule by showing what happens without it.
//
// Both halves are needed and neither alone is evidence. The ACCEPT half says the
// guard tolerates time.Time today, which is equally consistent with the guard
// never having reached it. The DISABLED half shows the type IS reached and IS
// judgeable, so the tolerance is the boundary rule doing work rather than the
// recursion falling short.
//
// Measured while building this, and the reason the rule ships despite the
// exception list being empty: with the boundary disabled the walk visits 8
// in-module struct types plus 4 more it should never have entered, and time.Time
// becomes a false positive. Zero-out-of-module-today is a fact about the current
// types, not a property of the guard.
func TestResolvedGuard_ModuleBoundaryRuleIsLoadBearing(t *testing.T) {
	t.Run("time.Time really does implement json.Marshaler", func(t *testing.T) {
		// If this ever goes false the control below is vacuous: it would be
		// passing because there is nothing to catch, not because of the rule.
		assert.True(t, implementsJSONMarshaler(reflect.TypeOf(time.Time{})),
			"the boundary-rule control depends on time.Time being a real "+
				"json.Marshaler; if it is not, this test proves nothing")
	})

	t.Run("boundary rule ON accepts the out-of-module type", func(t *testing.T) {
		assert.False(t, instrumentFires(t, assertNoCustomMarshaler, ctlOutOfModule{}),
			"time.Time is out of module and must be reached but not judged")
	})

	t.Run("boundary rule DISABLED turns it into a false positive", func(t *testing.T) {
		disabled := func(t *testing.T, v any) { walkMarshalerBan(t, v, true) }
		assert.True(t, instrumentFires(t, disabled, ctlOutOfModule{}),
			"with the boundary rule off, the first standard-library type reached "+
				"becomes a false positive — which is exactly what the rule buys. "+
				"If this row goes green the rule has stopped doing anything and "+
				"deleting it would be free, so check the walk still descends into "+
				"named struct fields before believing it.")
	})
}

// assertResponseKeySetIsTypeDetermined runs all three interveners against v.
//
// The three are bundled here rather than called separately at the use site so
// that TestResolvedSettingsGuard_InstrumentControls can drive them as a unit.
// Deleting any one of the three from this function turns control rows red;
// called individually at the use site, a deletion there would be invisible.
//
// KNOWN RESIDUAL, MEASURED, NOT CLOSED. Bundling narrows what a deletion can
// reach; it does not make the CALL to this function self-defending. Deleting
// the three-line loop in TestResolvedSettingsResponseShape_NoEffectiveValue
// leaves the ENTIRE pkg/hub suite green (rc=0) and is not a compile artefact
// (go vet clean; no orphaned range variable). Three lines removes every shape
// guard in this file and nothing goes red.
//
// The honest framing is that the composite made this residual SMALLER but not
// weaker: the same total-removal edit was six lines before the composite and
// is three lines now, and nothing caught the six-line version either. Coverage
// did not drop. Do not read the control table's green as evidence that the
// guards are still wired up — the table proves the instruments work, never
// that anything calls them.
//
// CORRECTION, this line previously read "closing this needs a check outside
// `go test`": that was FALSE, and it ruled out the class of fix that turned out
// to work. The residual IS closable inside `go test`, with resolvedResponseTypes
// as a swappable var — see TestResolvedSettingsGuard_InterveningIsWiredToTheResponseTypes,
// which poisons that var and asserts the subject test notices.
//
// The residual described above is therefore CLOSED. This paragraph stays because
// the reasoning is still load-bearing: the control table proves the instruments
// work and never that anything calls them, and that remains true. What changed is
// that a second test now supplies the missing half.
func assertResponseKeySetIsTypeDetermined(t *testing.T, v any) {
	t.Helper()

	assertOnlySafeTagOptions(t, v)      // intervener 1: tags
	assertNoEmbeddedStructPointer(t, v) // intervener 2: type structure
	assertNoCustomMarshaler(t, v)       // intervener 3: behaviour
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

type ctlOmitEmpty struct {
	ProjectSet bool `json:"projectSet"`
	ctlOmitTarget
}

// ctlOmitZero is the shape that was green at 25f82c33 while putting a key on the
// wire. It is the reason the tag check became an allowlist.
type ctlOmitZero struct {
	ProjectSet bool `json:"projectSet"`
	ctlZeroTarget
}

type ctlEmbeddedPtr struct {
	ProjectSet bool `json:"projectSet"`
	*ctlEmbedTarget
}

// ctlDoorA / ctlDoorB mirror the two doors that were measured OPEN against the
// real types before the marshaller ban recursed: hubclient.BucketConfig reached
// at .Project.Bucket (depth 2) and hubclient.ProjectResourceList reached at
// .Project.DefaultResources.Requests (depth 3).
//
// They are shape copies rather than the real types because the real ones carry
// no marshaller and must not be given one to satisfy a test. The depths are the
// measured depths, and the intermediate hop is a POINTER field in both, which is
// what the real graph does and what removes any addressability mitigation.
//
// NOTE ON WHAT THE REAL DOORS PROVED, which no committed row can restate: with a
// pointer-receiver MarshalJSON planted on hubclient.BucketConfig, "provider" and
// "name" left the wire. Neither is omitempty. The exposure is not confined to
// optional fields.
type ctlDoorLeaf struct {
	Provider string `json:"provider"`
}

func (*ctlDoorLeaf) MarshalJSON() ([]byte, error) { return []byte(`{"doorPwned":true}`), nil }

type ctlDoorAInner struct {
	Bucket *ctlDoorLeaf `json:"bucket,omitempty"`
}

// ctlDoorA: leaf marshaller two hops out.
type ctlDoorA struct {
	Project *ctlDoorAInner `json:"project"`
}

// ctlDoorLeafValue is ctlDoorLeaf with a VALUE receiver. Both receiver forms are
// pinned at depth because a real door can be written either way, and the walk must
// not depend on which one the author picked.
//
// WHAT THIS ROW DOES NOT DO, stated because the pairing invites the wrong reading:
// it does not isolate the value disjunct of implementsJSONMarshaler. The walk
// dereferences to a struct type before judging any node, and for a struct type T the
// method set of *T contains that of T, so the POINTER disjunct alone decides every
// node the walk ever reaches. Measured: deleting the value disjunct fails exactly one
// test in this package -- TestResolvedGuard_MarshalerCheckSeesBothReceiverForms,
// which calls the predicate directly -- and NO row of the control table, this one
// included. Deleting the pointer disjunct fails eight rows. The value disjunct is
// load-bearing for the predicate's pointer-typed inputs and redundant inside the
// walk; only the direct-predicate test pins it.
type ctlDoorLeafValue struct {
	Provider string `json:"provider"`
}

func (ctlDoorLeafValue) MarshalJSON() ([]byte, error) { return []byte(`{"doorPwned":true}`), nil }

type ctlDoorValueInner struct {
	Bucket *ctlDoorLeafValue `json:"bucket,omitempty"`
}

// ctlDoorValue: value-receiver leaf marshaller two hops out.
type ctlDoorValue struct {
	Project *ctlDoorValueInner `json:"project"`
}

type ctlDoorBSpec struct {
	Requests *ctlDoorLeaf `json:"requests,omitempty"`
}

type ctlDoorBInner struct {
	DefaultResources *ctlDoorBSpec `json:"defaultResources,omitempty"`
}

// ctlDoorB: leaf marshaller three hops out.
type ctlDoorB struct {
	Project *ctlDoorBInner `json:"project"`
}

// ctlOutOfModule reaches a standard-library type that implements json.Marshaler
// legitimately. The recursion must REACH time.Time and decline to judge it.
//
// This fixture is why resolvedGuardModulePrefix is not decorative: with the
// boundary rule disabled, this exact shape produces a false positive. That is
// pinned by TestResolvedGuard_ModuleBoundaryRuleIsLoadBearing rather than left
// to the comment, because "insurance against a type nobody has added yet" is
// the argument a future reader deletes.
type ctlOutOfModule struct {
	When time.Time `json:"when"`
}

// ctlDeepClean is ctlDoorB's shape with no marshaller anywhere. It is the
// over-rejection control for the recursion: without it, a ban that fired on
// every nested struct would pass all the REJECT rows above and look correct.
type ctlDeepCleanLeaf struct {
	Provider string `json:"provider"`
}

type ctlDeepCleanSpec struct {
	Requests *ctlDeepCleanLeaf `json:"requests,omitempty"`
}

type ctlDeepCleanInner struct {
	DefaultResources *ctlDeepCleanSpec `json:"defaultResources,omitempty"`
}

type ctlDeepClean struct {
	Project *ctlDeepCleanInner `json:"project"`
}

type ctlMid struct{ *ctlEmbedTarget }

// ctlNestedPtr hides the embedded pointer one level down, under an embedded
// VALUE. A top-level scan for anonymous pointer fields does not see it.
type ctlNestedPtr struct {
	ProjectSet bool `json:"projectSet"`
	ctlMid
}

type ctlEmbeddedValue struct {
	ProjectSet bool `json:"projectSet"`
	ctlEmbedTarget
}

type ctlNamedPtr struct {
	ProjectSet bool            `json:"projectSet"`
	Extra      *ctlEmbedTarget `json:"extra"`
}

// instrumentFires reports whether f fails when run against v.
//
// f is run against a throwaway *testing.T so a neighbouring assertion cannot
// manufacture the result — the failure mode that made two of this file's own
// manual controls go red for the wrong reason, tripping the exact-set and mirror
// assertions rather than the instrument under test.
//
// The goroutine is not incidental. The instruments use require.* for their
// structural preconditions, and require failing calls FailNow, which is
// runtime.Goexit. Run inline that would tear down the calling test goroutine
// mid-table; contained here, the deferred close still runs and the caller sees
// an ordinary "it failed".
func instrumentFires(t *testing.T, f func(*testing.T, any), v any) bool {
	t.Helper()

	// Every input must be a struct, because the instruments open with a
	// require on the kind. Without this line a non-struct row would trip that
	// require, sub.Failed() would report true, and a REJECT row would PASS —
	// recording the instrument as having caught a defect it never examined.
	// That is the red-for-the-wrong-reason failure this table exists to prevent,
	// so it would be a poor thing to reproduce inside the table itself.
	require.Equal(t, reflect.Struct, reflect.Indirect(reflect.ValueOf(v)).Kind(),
		"control inputs must be structs; the instruments require on kind and a "+
			"non-struct would be rejected as malformed rather than examined")

	sub := &testing.T{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f(sub, v)
	}()
	<-done

	return sub.Failed()
}

// TestResolvedSettingsGuard_InstrumentControls pins the instruments themselves.
//
// Before this existed the whole shape guard could be removed in two lines with
// the suite still green: deleting the pointer half of the marshaller check, or
// deleting the intervener calls outright, broke no test in either tag mode. The
// controls for those cases had been run by hand and were correct, but evidence
// that lives in a transcript does not fail a build, and the next person to
// simplify one of these conditions will have neither.
//
// The ACCEPT rows carry as much weight as the REJECT rows. A ban that rejects
// everything passes every REJECT row while making the guard useless, and a
// legitimate named-pointer or embedded-value field must keep working.
func TestResolvedSettingsGuard_InstrumentControls(t *testing.T) {
	const why = "\n\nThis control exists so the instrument cannot be weakened in " +
		"silence. If you are reading this because you simplified one of the " +
		"assert helpers, the simplification removed coverage — see the coverage " +
		"argument on resolvedResponseTypes before changing it back."

	cases := []struct {
		name string
		f    func(*testing.T, any)
		v    any
		want bool
	}{
		// REJECT rows: intervener 1, tags.
		{"omitempty promoted from embedded value", assertOnlySafeTagOptions, ctlOmitEmpty{}, true},
		{"omitzero promoted from embedded value", assertOnlySafeTagOptions, ctlOmitZero{}, true},

		// REJECT rows: intervener 2, type structure.
		{"embedded pointer at top level", assertNoEmbeddedStructPointer, ctlEmbeddedPtr{}, true},
		{"embedded pointer one level down", assertNoEmbeddedStructPointer, ctlNestedPtr{}, true},

		// REJECT rows: intervener 3, behaviour. The pointer-receiver row is the
		// form production uses; without it, deleting the pointer half of the
		// check breaks nothing.
		{"value-receiver MarshalJSON", assertNoCustomMarshaler, marshalerCtrlValue{}, true},
		{"pointer-receiver MarshalJSON", assertNoCustomMarshaler, marshalerCtrlPointer{}, true},

		// REJECT rows: intervener 3 AT DEPTH. These are the two doors that were
		// measured OPEN on the real types — BucketConfig at depth 2 and
		// ProjectResourceList at depth 3 — with every package in ./pkg/... green
		// under both tag modes. A top-level-only ban passes both of these rows,
		// which is exactly why the acceptance criterion for the recursion was
		// these two going red rather than the suite staying green.
		{"marshaller two hops out", assertNoCustomMarshaler, ctlDoorA{}, true},
		{"marshaller three hops out", assertNoCustomMarshaler, ctlDoorB{}, true},
		{"composite catches depth", assertResponseKeySetIsTypeDetermined, ctlDoorB{}, true},

		// REJECT rows through the COMPOSITE. These are what make deleting one of
		// the three calls inside assertResponseKeySetIsTypeDetermined visible.
		{"composite catches tags", assertResponseKeySetIsTypeDetermined, ctlOmitZero{}, true},
		{"composite catches structure", assertResponseKeySetIsTypeDetermined, ctlNestedPtr{}, true},
		{"composite catches behaviour", assertResponseKeySetIsTypeDetermined, marshalerCtrlPointer{}, true},

		// POINTER-TYPE inputs. Every row above passes a VALUE, which is what
		// resolvedResponseTypes supplies today.
		//
		// THIS BLOCK'S ORIGINAL JUSTIFICATION IS NO LONGER TRUE, AND THE ROWS ARE
		// KEPT FOR A DIFFERENT REASON. It used to read: "assertNoCustomMarshaler
		// does not dereference, so its two halves swap which one carries the
		// check depending on the input form." That was accurate when the ban was
		// a single top-level check. It is FALSE now — walkMarshalerBan strips
		// Pointer/Slice/Array/Map before judging, so every node reaching the
		// predicate from the walk is a struct kind, and for struct T the method
		// set of *T is a superset of T's. The pointer half alone decides every
		// node the walk visits.
		//
		// So for the three MARSHALLER rows below, value and pointer inputs now
		// pin the SAME half, and the old payoff line — "pinning both forms means
		// whichever half carries it is covered" — is false for them. Measured:
		// deleting the value disjunct reds ZERO rows of this table.
		//
		// WHAT THEY STILL PIN, which is a real property and not a consolation:
		// that the walk NORMALIZES. A pointer input and a value input must reach
		// the same verdict, and these rows fail if someone removes the
		// dereference loop or makes it partial.
		//
		// Two of the five rows are UNAFFECTED by any of this: "embedded pointer"
		// and "omitzero" route to assertNoEmbeddedStructPointer and
		// assertOnlySafeTagOptions, which were not changed and do not dereference.
		//
		// The comment this replaces predicted its own failure mode — "a green
		// table that has quietly stopped testing what runs" — and then became an
		// instance of it. Coverage of the value disjunct went from four rows to
		// one when the walk began dereferencing. That one is
		// TestResolvedGuard_MarshalerCheckSeesBothReceiverForms, which calls the
		// predicate directly; delete the disjunct and that subtest together and
		// the whole package is green. Do not delete either. The disjunct is
		// correct and required for pointer-typed inputs handed to the predicate
		// outside the walk; removing it is a false negative in the R21 class.
		{"pointer input, pointer-receiver MarshalJSON", assertNoCustomMarshaler, &marshalerCtrlPointer{}, true},
		{"pointer input, value-receiver MarshalJSON", assertNoCustomMarshaler, &marshalerCtrlValue{}, true},
		{"pointer input, embedded pointer", assertNoEmbeddedStructPointer, &ctlEmbeddedPtr{}, true},
		{"pointer input, omitzero", assertOnlySafeTagOptions, &ctlOmitZero{}, true},
		{"pointer input, composite catches behaviour", assertResponseKeySetIsTypeDetermined, &marshalerCtrlPointer{}, true},

		// ACCEPT rows.
		{"named pointer field is ACCEPTED", assertNoEmbeddedStructPointer, ctlNamedPtr{}, false},
		{"embedded value is ACCEPTED", assertNoEmbeddedStructPointer, ctlEmbeddedValue{}, false},
		{"plain tags are ACCEPTED", assertOnlySafeTagOptions, ctlEmbeddedValue{}, false},
		{"no marshaller is ACCEPTED", assertNoCustomMarshaler, ctlEmbeddedValue{}, false},
		{"a clean type passes the composite", assertResponseKeySetIsTypeDetermined, ctlClean{}, false},
		{"a clean POINTER passes the composite", assertResponseKeySetIsTypeDetermined, &ctlClean{}, false},

		// DEPTH rows, REJECT. These were filed under the ACCEPT heading above by
		// mistake; a row's want is in its last field, not in the heading it sits
		// under, so this misfiling could not change a result — but a reader
		// scanning headings would have miscounted the accepts, which is how a
		// table stops being read carefully.
		{"nested POINTER-receiver marshaller is rejected", assertNoCustomMarshaler, ctlDoorA{}, true},
		{"nested VALUE-receiver marshaller is rejected", assertNoCustomMarshaler, ctlDoorValue{}, true},

		// DEPTH rows, ACCEPT — the over-rejection controls for the two above.
		{"nested structs with no marshaller anywhere", assertNoCustomMarshaler, ctlDeepClean{}, false},
		{"out-of-module marshaller is reached but not judged", assertNoCustomMarshaler, ctlOutOfModule{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := instrumentFires(t, tc.f, tc.v); got != tc.want {
				t.Errorf("instrument fired=%v, want %v%s", got, tc.want, why)
			}
		})
	}
}

// TestResolvedSettings_SettingsValueTypeIdentity pins the map's value type.
//
// The coverage argument on resolvedResponseTypes reasons about four types. That
// reasoning only reaches the "settings" sub-objects because the map's value type
// IS one of the four; widen it and the response grows a shape no guard in this
// file has looked at.
//
// The property was already held before this test existed — but only by the
// compiler, incidentally, at the map's two construction sites, with no message
// attached. That is a worse position than it sounds, because the field
// immediately above it, Project, has a named test explaining that retyping it is
// the supported way to sneak an effective value into this response. The
// asymmetry reads as "this class was considered and Settings was found not to
// need it," which is not true and is not what anyone decided.
func TestResolvedSettings_SettingsValueTypeIdentity(t *testing.T) {
	field, ok := reflect.TypeOf(ResolvedProjectSettings{}).FieldByName("Settings")
	require.True(t, ok, "ResolvedProjectSettings has no Settings field")
	require.Equal(t, reflect.Map, field.Type.Kind(),
		"Settings must stay a map; the guards below assume a map value type")

	assert.Equal(t, reflect.TypeOf(ResolvedProjectSetting{}), field.Type.Elem(),
		"the resolved response's \"settings\" map must stay keyed to exactly "+
			"ResolvedProjectSetting.\n"+
			"Widening this value type — embedding ResolvedProjectSetting in a richer "+
			"struct, say — moves the per-setting object outside the set of types the "+
			"shape guard checks, and a widened entry is precisely how an effective "+
			"value would reach clients. That is the thing these guards exist to "+
			"prevent, and it is why this is asserted rather than left to the two "+
			"construction sites where the compiler happens to notice.")
}
