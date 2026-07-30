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
// At the time of writing that is 10 of 17 invisible.
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
// `omitempty` would defeat this helper: it drops a zero-valued field, so a key
// could exist on the type, be populated by the handler, reach the wire, and
// still be missing from the set this helper computes for a test-constructed
// value. Callers must therefore pair it with assertNoPromotedOmitEmpty, which
// removes the possibility rather than trying to dodge it. See that function for
// why the obvious alternative — populating the value first — does not work.
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

// assertNoPromotedOmitEmpty fails if any field promoted onto v's top-level JSON
// object carries `,omitempty`.
//
// This is what makes jsonWireNames trustworthy on a zero value, and it is the
// second half of the fix — without it the wire guard has a hole the same shape
// as the one it was written to close.
//
// THE ALTERNATIVE WAS TRIED AND MEASURED. The obvious way to beat `omitempty`
// is to populate the value before marshalling it, reflectively so that fields
// the test does not know about are filled too. That does not work, and it fails
// on precisely the case this guard exists for: a field promoted from an
// embedded UNEXPORTED struct is not settable through reflect (CanSet is false,
// and there is no way around it short of unsafe). Measured on the real type,
// with the encoder as the judge:
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
// Banning `omitempty` outright is the honest version, and it costs nothing
// here: this response never omits keys by design. Every registered setting
// appears every time, and an unset projectValue is explicit `null` rather than
// a missing key, precisely so a client cannot read "absent" out of silence.
// `omitempty` on these types would contradict the documented contract before it
// ever troubled a test.
func assertNoPromotedOmitEmpty(t *testing.T, v any) {
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
			// the type of an unexported embedded field even though it cannot
			// set its value, which is what lets this check reach where a
			// reflective fill could not.
			if field.Anonymous && parts[0] == "" {
				ft := field.Type
				for ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if ft.Kind() == reflect.Struct {
					walk(ft, path+field.Name+".")
					continue
				}
			}

			for _, opt := range parts[1:] {
				assert.NotEqualf(t, "omitempty", opt,
					"%s%s carries `omitempty`, which is not allowed on the "+
						"resolved-settings response types.\n"+
						"Two reasons, and the second is the one that bites:\n"+
						"  1. This response never omits keys. Every registered setting is "+
						"reported every time, and an unset projectValue is explicit null, "+
						"so that a client cannot infer \"absent\" from a missing key.\n"+
						"  2. `omitempty` hides a zero-valued field from the encoder, and "+
						"the shape guard reads the encoder's output. A field with "+
						"`omitempty` can reach real clients while "+
						"TestResolvedSettingsResponseShape_NoEffectiveValue stays green.",
					path, field.Name)
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
				"projectSettingKeys. Either it was removed from the registry (delete the "+
				"descriptor too) or it was never a project setting (the endpoint must not "+
				"report it).",
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
// the "project" key carries whole. A field added to ProjectSettings reaches
// this response unguarded by this test. Exact-setting that type here was
// considered and rejected: it has nineteen fields that legitimately change for
// reasons unrelated to this endpoint, so this guard would fail on other
// people's honest work, and a guard that cries wolf gets deleted. The realistic
// version of that drift — the "project" field being retyped to a locally
// widened struct — is covered by TestResolvedSettings_ProjectFieldTypeIdentity
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
	// Step 1: no promoted field may hide from the encoder. Without this the
	// step below can be defeated by six lines; see assertNoPromotedOmitEmpty.
	for _, v := range []any{
		ResolvedProjectSettings{}, ResolvedProjectSetting{},
		hubclient.ResolvedProjectSettings{}, hubclient.ResolvedProjectSetting{},
	} {
		assertNoPromotedOmitEmpty(t, v)
	}

	// Step 2: the encoder's actual output is exactly the expected set. Safe on
	// zero values only because of step 1 — encoding/json emits a key for every
	// field it knows about unless omitempty tells it not to.
	assert.Equal(t, expectedResolvedWrapperFields,
		jsonWireNames(t, ResolvedProjectSettings{}),
		"hub.ResolvedProjectSettings"+rationale)
	assert.Equal(t, expectedResolvedEntryFields,
		jsonWireNames(t, ResolvedProjectSetting{}),
		"hub.ResolvedProjectSetting"+rationale)

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
		"boundary, and this assertion is the only thing that notices when they diverge."

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
