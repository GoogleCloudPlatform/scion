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

// jsonTagNames returns the JSON field names of a struct type, following the
// wire contract rather than the Go identifiers: the tag name is used, tag
// options such as ",omitempty" are stripped, fields tagged "-" are excluded,
// and an untagged field contributes its Go name (which is what encoding/json
// would emit).
func jsonTagNames(t *testing.T, v any) []string {
	t.Helper()

	typ := reflect.TypeOf(v)
	for typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	require.Equalf(t, reflect.Struct, typ.Kind(),
		"jsonTagNames expects a struct, got %s", typ.Kind())

	var names []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath != "" {
			continue // unexported: never marshalled
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
// It asserts on JSON TAGS rather than Go field names because the wire shape is
// the contract: renaming a Go field while keeping its tag must not trip this,
// and changing a tag must.
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

	// The expected sets. A legitimate new field is added HERE, in one obvious
	// place — the guard is meant to be a speed bump that forces an argument,
	// not a wall that gets deleted by the first person it inconveniences.
	expectedWrapper := []string{"project", "settings"}
	expectedEntry := []string{"hubDefault", "projectSet", "projectValue"}
	sort.Strings(expectedWrapper)
	sort.Strings(expectedEntry)

	assert.Equal(t, expectedWrapper, jsonTagNames(t, ResolvedProjectSettings{}),
		"hub.ResolvedProjectSettings"+rationale)
	assert.Equal(t, expectedEntry, jsonTagNames(t, ResolvedProjectSetting{}),
		"hub.ResolvedProjectSetting"+rationale)

	// The hubclient mirror is part of the same contract: a field added to only
	// one side would produce a client type that silently disagrees with the
	// server.
	assert.Equal(t, expectedWrapper, jsonTagNames(t, hubclient.ResolvedProjectSettings{}),
		"hubclient.ResolvedProjectSettings"+rationale)
	assert.Equal(t, expectedEntry, jsonTagNames(t, hubclient.ResolvedProjectSetting{}),
		"hubclient.ResolvedProjectSetting"+rationale)
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
