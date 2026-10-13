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
	"math"
	"reflect"
	"testing"
	"unsafe"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetVersionedSettingValue(t *testing.T) {
	autohelp := true
	enabled := false
	linked := true
	localOnly := true

	vs := &VersionedSettings{
		SchemaVersion:        "1",
		ActiveProfile:        "staging",
		DefaultTemplate:      "my-template",
		DefaultHarnessConfig: "claude",
		ImageRegistry:        "ghcr.io/myorg",
		CLI:                  &V1CLIConfig{AutoHelp: &autohelp},
		Hub: &V1HubClientConfig{
			Enabled:   &enabled,
			Linked:    &linked,
			Endpoint:  "https://hub.example.com",
			ProjectID: "project-123",
			LocalOnly: &localOnly,
		},
		Server: &V1ServerConfig{
			Broker: &V1BrokerConfig{
				BrokerID:       "broker-1",
				BrokerToken:    "tok-secret",
				BrokerNickname: "my-broker",
			},
		},
	}

	tests := []struct {
		key  string
		want string
	}{
		{"active_profile", "staging"},
		{"default_template", "my-template"},
		{"default_harness_config", "claude"},
		{"image_registry", "ghcr.io/myorg"},
		{"cli.autohelp", "true"},
		{"project_id", "project-123"},
		{"hub.enabled", "false"},
		{"hub.linked", "true"},
		{"hub.endpoint", "https://hub.example.com"},
		{"hub.projectId", "project-123"},
		{"hub.local_only", "true"},
		{"hub.brokerId", "broker-1"},
		{"hub.brokerToken", "tok-secret"},
		{"hub.brokerNickname", "my-broker"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got, err := GetVersionedSettingValue(vs, tt.key)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	// Unknown key should error
	_, err := GetVersionedSettingValue(vs, "nonexistent_key")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown or complex setting key")

	// Legacy grove_id key names are no longer accepted as CLI key-name input.
	for _, key := range []string{"grove_id", "hub.grove_id", "hub.groveId"} {
		_, err := GetVersionedSettingValue(vs, key)
		assert.Error(t, err, "key=%s", key)
		assert.Contains(t, err.Error(), "unknown or complex setting key", "key=%s", key)
	}

	// Nil sub-structs should return empty strings
	empty := &VersionedSettings{SchemaVersion: "1"}
	for _, key := range []string{"project_id", "hub.endpoint", "hub.brokerId", "cli.autohelp"} {
		got, err := GetVersionedSettingValue(empty, key)
		require.NoError(t, err, "key=%s", key)
		assert.Empty(t, got, "key=%s", key)
	}
}

func TestGetVersionedSettingValueNestedMaps(t *testing.T) {
	vs := &VersionedSettings{
		SchemaVersion: "1",
		Profiles: map[string]V1ProfileConfig{
			"staging": {
				Runtime:       "docker",
				ImageRegistry: "ghcr.io/myorg",
				HarnessOverrides: map[string]V1HarnessOverride{
					"claude": {Image: "override-image"},
				},
			},
		},
		Runtimes: map[string]V1RuntimeConfig{
			"local": {
				Type:              "docker",
				Namespace:         "ns1",
				GKE:               true,
				ListAllNamespaces: false,
				Env:               map[string]string{"FOO": "bar"},
				CloudRun:          &CloudRunConfig{Location: "us-central1"},
			},
			// "bare" deliberately leaves CloudRun (and the other pointer-to-struct
			// fields) unset, to test that an unset structured field is refused the
			// same way as a set one — see the nil-pointer-to-struct cases below.
			"bare": {
				Type: "kubernetes",
			},
		},
		HarnessConfigs: map[string]HarnessConfigEntry{
			"claude": {
				Harness: "claude",
				Image:   "myimage",
				Secrets: []api.RequiredSecret{{Key: "TOKEN"}},
			},
		},
	}

	scalarTests := []struct {
		key  string
		want string
	}{
		{"profiles.staging.runtime", "docker"},
		{"profiles.staging.image_registry", "ghcr.io/myorg"},
		{"profiles.staging.default_template", ""},
		{"runtimes.local.type", "docker"},
		{"runtimes.local.namespace", "ns1"},
		{"runtimes.local.gke", "true"},
		{"runtimes.local.list_all_namespaces", "false"},
	}
	for _, tt := range scalarTests {
		t.Run(tt.key, func(t *testing.T) {
			got, err := GetVersionedSettingValue(vs, tt.key)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	errTests := []struct {
		name    string
		key     string
		wantErr string
	}{
		{"unknown profile name", "profiles.missing.runtime", `no profile named "missing"`},
		{"unknown runtime name", "runtimes.missing.type", `no runtime named "missing"`},
		{"unknown field on profile", "profiles.staging.nope", `has no field "nope"`},
		{"unknown field on runtime", "runtimes.local.nope", `has no field "nope"`},
		{"non-scalar profile field", "profiles.staging.harness_overrides", "is not a scalar value"},
		{"non-scalar runtime field", "runtimes.local.env", "is not a scalar value"},
		// A pointer-to-struct field that is nil (unset) must be refused exactly
		// like one that is set — scalar-ness is decided by the pointee's static
		// type, not by whether the value happens to be nil. Treating every nil
		// pointer as scalar would return an empty string with a nil error, so
		// the CLI would print an empty line and exit 0 for a structured field.
		{"nil pointer-to-struct profile field", "profiles.staging.resources", "is not a scalar value"},
		{"nil pointer-to-struct runtime field", "runtimes.bare.cloudrun", "is not a scalar value"},
		{"nested path under a nil pointer-to-struct field", "runtimes.bare.cloudrun.location", `field "cloudrun" of runtime "bare" is not a scalar value; nested paths are not supported`},
		{"unsupported category falls back to generic error", "widgets.a.b", "unknown or complex setting key: widgets.a.b"},
		{"too few parts falls back to generic error", "profiles.staging", "unknown or complex setting key: profiles.staging"},
		// harness_configs is deliberately unsupported (see getNestedMapSettingValue's
		// doc comment): it falls through to the same flat "unknown key" error as any
		// other unrecognized key, rather than a nested-map lookup error, since
		// GetVersionedSettingValue never dispatches to lookupMapEntryField for it.
		{"harness_configs category is not supported", "harness_configs.claude.image", "unknown or complex setting key: harness_configs.claude.image"},
		// Nested paths below a resolved field report a "not supported" error naming
		// the resolved field, not a misleading "no such field" against the full
		// dotted remainder — whether that field is itself structured (cloudrun, a
		// nested struct) or scalar (namespace, a plain string with no sub-fields).
		{"nested path into a structured field", "runtimes.local.cloudrun.location", `field "cloudrun" of runtime "local" is not a scalar value; nested paths are not supported`},
		{"nested path into a scalar field", "runtimes.local.namespace.sub", `field "namespace" of runtime "local" is a scalar value and has no sub-fields; nested paths are not supported`},
		// An unresolved first segment of a nested path names only that segment
		// ("nope", not the full "nope.sub" remainder) — the field that is
		// actually missing — and notes that nested paths are not supported,
		// rather than implying "nope" exists and only "sub" is the problem.
		{"nested path with unknown first segment", "profiles.staging.nope.sub", `has no field "nope"; nested paths are not supported`},
		// findFieldByConfigTag matches the configTagName exactly: a field name
		// that merely extends, or differs only in case from, a real tag does
		// not match it.
		{"field name extending a real tag", "runtimes.local.namespacex", `has no field "namespacex"`},
		{"field name in a different case", "runtimes.local.NAMESPACE", `has no field "NAMESPACE"`},
		// A trailing dot leaves the field segment empty; findFieldByConfigTag's
		// tag != "" guard (see TestLookupScalarFieldEmptyNameMatchesNoField)
		// keeps that empty name from matching any field.
		{"empty field segment", "profiles.staging.", `has no field ""`},
	}
	for _, tt := range errTests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := GetVersionedSettingValue(vs, tt.key)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.key)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestCredentialLikeFieldPatternAlternatives pins credentialLikeFieldPattern
// itself, one matching name per alternative (plus a mixed-case one, to pin
// the (?i) flag) and a set of names that must not match. Without this, a
// mutation that drops most of the pattern's alternatives — leaving only
// enough to satisfy the other tests, which each reference only one or two
// specific names — passes undetected; see TestLookupScalarFieldRefusesCredentialLikeNames
// and TestScalarFieldsExcludeCredentialLikeNames, neither of which touches
// most of these alternatives.
func TestCredentialLikeFieldPatternAlternatives(t *testing.T) {
	matches := []string{
		"token",
		"client_secret",
		"password",
		"passwd",
		"api_key",
		"apikey",
		"credential",
		"gh_pat",
		"pat", // bare "pat", pinning ^pat$
		"private_key",
		"privatekey",
		"ssh_key",
		"sshkey",
		"bearer",
		"auth_header",
		"authheader",
		"API_KEY", // mixed case, pins the (?i) flag
	}
	for _, name := range matches {
		t.Run("matches/"+name, func(t *testing.T) {
			assert.True(t, credentialLikeFieldPattern.MatchString(name))
		})
	}

	nonMatches := []string{
		// The real scalar tags on V1ProfileConfig and V1RuntimeConfig — must
		// never be refused, or config get would break for every user.
		"runtime",
		"namespace",
		"default_template",
		"image_registry",
		"list_all_namespaces",
		// A documented gap (see credentialLikeFieldPattern's comment): a
		// credential-holding field under this name would still render in
		// plaintext, since name-based coverage cannot catch it.
		"passphrase",
		// A word that merely ends in "pat" is not a credential-like name.
		// The pattern anchors on "_pat$" or "^pat$" rather than a bare
		// "pat$", specifically to exclude names like this.
		"compat",
	}
	for _, name := range nonMatches {
		t.Run("does_not_match/"+name, func(t *testing.T) {
			assert.False(t, credentialLikeFieldPattern.MatchString(name))
		})
	}
}

// TestLookupScalarFieldRefusesCredentialLikeNames directly exercises the
// credential-name refusal branch of lookupScalarField, using a local
// struct with a plain string field whose tag matches
// credentialLikeFieldPattern. This proves the refusal actually fires (not
// just that no current production field happens to match) — see
// TestScalarFieldsExcludeCredentialLikeNames for the production-struct
// guard.
func TestLookupScalarFieldRefusesCredentialLikeNames(t *testing.T) {
	type fakeEntry struct {
		APIKey string `koanf:"api_key"`
		SSHKey string `koanf:"ssh_key"`
		Name   string `koanf:"name"`
	}
	entry := fakeEntry{APIKey: "super-secret-value", SSHKey: "another-secret", Name: "ok"}

	for _, field := range []string{"api_key", "ssh_key"} {
		_, err := lookupScalarField(entry, field, "widgets.x."+field, "widget", "x")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "looks like a credential")
		assert.NotContains(t, err.Error(), "secret-value")
		assert.NotContains(t, err.Error(), "another-secret")
	}

	val, err := lookupScalarField(entry, "name", "widgets.x.name", "widget", "x")
	require.NoError(t, err)
	assert.Equal(t, "ok", val)
}

// TestLookupScalarFieldNilPointerClassification pins scalarValueString's
// nil-pointer handling using a local struct with fields V1ProfileConfig and
// V1RuntimeConfig don't happen to exercise today: a nil pointer to a scalar
// type (e.g. *bool) must still render as an empty string, while a nil
// pointer to a struct (e.g. an unset CloudRun-shaped field) must be refused
// as non-scalar — scalar-ness is decided by the pointee's static type, not
// by whether the field happens to be nil.
func TestLookupScalarFieldNilPointerClassification(t *testing.T) {
	type fakeStruct struct {
		Sub string `koanf:"sub"`
	}
	type fakeEntry struct {
		NilString *string     `koanf:"nil_string"`
		NilBool   *bool       `koanf:"nil_bool"`
		NilStruct *fakeStruct `koanf:"nil_struct"`
	}
	entry := fakeEntry{}

	val, err := lookupScalarField(entry, "nil_string", "widgets.x.nil_string", "widget", "x")
	require.NoError(t, err)
	assert.Empty(t, val)

	val, err = lookupScalarField(entry, "nil_bool", "widgets.x.nil_bool", "widget", "x")
	require.NoError(t, err)
	assert.Empty(t, val)

	_, err = lookupScalarField(entry, "nil_struct", "widgets.x.nil_struct", "widget", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not a scalar value")

	_, err = lookupScalarField(entry, "nil_struct.sub", "widgets.x.nil_struct.sub", "widget", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `field "nil_struct" of widget "x" is not a scalar value; nested paths are not supported`)
}

// TestScalarKindAgreesWithScalarValueString checks, for a representative
// type of every reflect.Kind that can appear as a struct field (i.e. every
// kind except Invalid and Pointer itself — pointer indirection is exercised
// via the PointerTo(t) case below, not as its own "kind" of leaf value),
// that isScalarKind(k) agrees with what scalarValueString actually does:
// once for a non-nil zero value of that kind, and once for a nil pointer to
// that kind (the path scalarValueString takes for an unset pointer field).
//
// The expected scalar-ness of each kind is pinned explicitly in the table
// (the "scalar" column below), not derived from isScalarKind itself: a
// table that instead computed want := isScalarKind(tt.typ.Kind()) could
// only ever catch disagreement between isScalarKind and scalarValueString,
// never a change to isScalarKind's own kind list (e.g. dropping the Uint
// kinds), since both sides of the comparison would move together. The
// completeness check at the end of the test asserts that every kind from
// reflect.Bool through reflect.UnsafePointer (except Pointer) has a row in
// the table, so a kind added to Go later, or a row removed here, cannot be
// skipped silently. isScalarKind is the single source of truth
// scalarValueString gates on for both paths, so this test is what makes
// "the two cannot drift" true rather than aspirational — see isScalarKind's
// doc comment.
func TestScalarKindAgreesWithScalarValueString(t *testing.T) {
	types := []struct {
		name   string
		typ    reflect.Type
		scalar bool
	}{
		{"string", reflect.TypeOf(""), true},
		{"bool", reflect.TypeOf(false), true},
		{"int", reflect.TypeOf(int(0)), true},
		{"int8", reflect.TypeOf(int8(0)), true},
		{"int16", reflect.TypeOf(int16(0)), true},
		{"int32", reflect.TypeOf(int32(0)), true},
		{"int64", reflect.TypeOf(int64(0)), true},
		{"uint", reflect.TypeOf(uint(0)), true},
		{"uint8", reflect.TypeOf(uint8(0)), true},
		{"uint16", reflect.TypeOf(uint16(0)), true},
		{"uint32", reflect.TypeOf(uint32(0)), true},
		{"uint64", reflect.TypeOf(uint64(0)), true},
		{"uintptr", reflect.TypeOf(uintptr(0)), false},
		{"float32", reflect.TypeOf(float32(0)), false},
		{"float64", reflect.TypeOf(float64(0)), false},
		{"complex64", reflect.TypeOf(complex64(0)), false},
		{"complex128", reflect.TypeOf(complex128(0)), false},
		{"struct", reflect.TypeOf(struct{}{}), false},
		{"map", reflect.TypeOf(map[string]int{}), false},
		{"slice", reflect.TypeOf([]int{}), false},
		{"array", reflect.TypeOf([1]int{}), false},
		{"interface", reflect.TypeOf((*any)(nil)).Elem(), false},
		{"chan", reflect.TypeOf(make(chan int)), false},
		{"func", reflect.TypeOf(func() {}), false},
		{"unsafe_pointer", reflect.TypeOf(unsafe.Pointer(nil)), false},
	}

	seen := make(map[reflect.Kind]bool, len(types))
	for _, tt := range types {
		seen[tt.typ.Kind()] = true
	}
	for k := reflect.Bool; k <= reflect.UnsafePointer; k++ {
		if k == reflect.Pointer {
			continue
		}
		assert.True(t, seen[k], "reflect.Kind %s has no row in the agreement table", k)
	}

	for _, tt := range types {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.scalar, isScalarKind(tt.typ.Kind()), "isScalarKind(%s)", tt.name)

			_, directOK := scalarValueString(reflect.Zero(tt.typ))
			assert.Equal(t, tt.scalar, directOK, "direct (non-pointer) %s value", tt.name)

			nilPtr := reflect.Zero(reflect.PointerTo(tt.typ))
			_, nilPtrOK := scalarValueString(nilPtr)
			assert.Equal(t, tt.scalar, nilPtrOK, "nil pointer to %s", tt.name)
		})
	}
}

// TestScalarValueStringRendersIntegers proves scalarValueString actually
// formats non-zero integer values correctly, not just that it accepts them
// (TestScalarKindAgreesWithScalarValueString only ever checks the zero
// value). It covers a negative int, the largest uint64, and a non-nil
// pointer to an int, through lookupScalarField so the credential-name and
// nested-path logic sits in the path too.
func TestScalarValueStringRendersIntegers(t *testing.T) {
	n := -7
	type fakeEntry struct {
		Neg    int    `koanf:"neg"`
		Big    uint64 `koanf:"big"`
		PtrInt *int   `koanf:"ptr_int"`
	}
	entry := fakeEntry{
		Neg:    -7,
		Big:    math.MaxUint64,
		PtrInt: &n,
	}

	tests := []struct {
		field string
		want  string
	}{
		{"neg", "-7"},
		{"big", "18446744073709551615"},
		{"ptr_int", "-7"},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			got, err := lookupScalarField(entry, tt.field, "widgets.x."+tt.field, "widget", "x")
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestConfigTagName exercises configTagName's yaml fallback and its "-"
// skip against a local struct, since no field on V1ProfileConfig or
// V1RuntimeConfig takes either path today — see configTagName's doc
// comment, which names this test.
func TestConfigTagName(t *testing.T) {
	type fakeStruct struct {
		KoanfOnly string `koanf:"koanf_only"`
		YamlOnly  string `yaml:"yaml_only,omitempty"`
		KoanfWins string `koanf:"koanf_wins" yaml:"yaml_loses"`
		Skipped   string `koanf:"-"`
		Untagged  string
	}

	tests := []struct {
		field string
		want  string
	}{
		{"KoanfOnly", "koanf_only"},
		{"YamlOnly", "yaml_only"},
		{"KoanfWins", "koanf_wins"},
		{"Skipped", ""},
		{"Untagged", ""},
	}

	typ := reflect.TypeOf(fakeStruct{})
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			f, ok := typ.FieldByName(tt.field)
			require.True(t, ok, "field %s not found on fakeStruct", tt.field)
			assert.Equal(t, tt.want, configTagName(f))
		})
	}
}

// TestLookupScalarFieldEmptyNameMatchesNoField proves that an empty field
// name — as reached via a trailing dot in a dotted key, e.g.
// "profiles.ci." — never resolves, even against a struct with an untagged
// field and a koanf:"-" field, both of which configTagName reports as ""
// the same way an empty query name is. findFieldByConfigTag's tag != ""
// guard is what keeps those two empty strings from being treated as a
// match; without it, the first untagged or "-" field would be returned for
// any empty name. No field on V1ProfileConfig or V1RuntimeConfig is
// untagged today, so this case is only reachable through a local struct.
func TestLookupScalarFieldEmptyNameMatchesNoField(t *testing.T) {
	type fakeStruct struct {
		Skipped  string `koanf:"-"`
		Untagged string
	}
	_, err := lookupScalarField(fakeStruct{}, "", "widgets.x.", "widget", "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `has no field ""`)
}

// TestFindFieldByConfigTagHandlesNonStructValues proves findFieldByConfigTag
// returns false instead of panicking when v is not a struct — a nil
// pointer, a non-nil pointer to a non-struct, or a plain non-struct value —
// since reflect.Type.NumField panics for non-struct kinds. Neither
// V1ProfileConfig nor V1RuntimeConfig is ever passed in as a pointer today
// (lookupMapEntryField's map values are plain structs), so this guards
// against a future caller or refactor passing one of these shapes rather
// than a currently reachable production path. A pointer to a struct is also
// checked, to prove the function still resolves through one level (or more)
// of indirection rather than refusing every pointer.
func TestFindFieldByConfigTagHandlesNonStructValues(t *testing.T) {
	type fakeStruct struct {
		Name string `koanf:"name"`
	}

	t.Run("nil pointer to struct", func(t *testing.T) {
		var p *fakeStruct
		_, ok := findFieldByConfigTag(reflect.ValueOf(p), "name")
		assert.False(t, ok)
	})
	t.Run("non-struct value", func(t *testing.T) {
		_, ok := findFieldByConfigTag(reflect.ValueOf("not a struct"), "name")
		assert.False(t, ok)
	})
	t.Run("nil pointer to non-struct", func(t *testing.T) {
		var p *string
		_, ok := findFieldByConfigTag(reflect.ValueOf(p), "name")
		assert.False(t, ok)
	})
	t.Run("pointer to struct resolves like the struct itself", func(t *testing.T) {
		entry := &fakeStruct{Name: "ok"}
		fv, ok := findFieldByConfigTag(reflect.ValueOf(entry), "name")
		require.True(t, ok)
		assert.Equal(t, "ok", fv.String())
	})
	t.Run("pointer to pointer to struct resolves through both", func(t *testing.T) {
		entry := &fakeStruct{Name: "ok"}
		pp := &entry
		fv, ok := findFieldByConfigTag(reflect.ValueOf(pp), "name")
		require.True(t, ok)
		assert.Equal(t, "ok", fv.String())
	})
}

// TestGetNestedMapSettingValueNilSettings proves getNestedMapSettingValue
// returns handled=false for a nil *VersionedSettings on a key shaped like
// "profiles.x.y", rather than panicking on vs.Profiles — and that
// GetVersionedSettingValue, which calls it, surfaces the ordinary "unknown
// or complex setting key" error for that case instead of panicking.
func TestGetNestedMapSettingValueNilSettings(t *testing.T) {
	_, err, handled := getNestedMapSettingValue(nil, "profiles.local.runtime")
	assert.False(t, handled)
	assert.NoError(t, err)

	_, err = GetVersionedSettingValue(nil, "profiles.local.runtime")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown or complex setting key: profiles.local.runtime")
}

// isScalarFieldType reports whether a struct field's static type is one
// lookupScalarField could actually render (string, bool, integer, or a
// pointer to one) — as opposed to a map, slice, or (pointer-to-)struct,
// which lookupScalarField already refuses via scalarValueString regardless
// of its name. Used by TestScalarFieldsExcludeCredentialLikeNames to scope
// the credential-name guard to fields that config get could actually print.
//
// This delegates to the production isScalarKind/underlyingKind helpers
// (also used by scalarValueString to classify a nil pointer-to-struct field,
// e.g. an unset CloudRun, as non-scalar) rather than re-deriving its own
// kind list, so this test-only classification cannot silently drift out of
// sync with what config get actually renders.
func isScalarFieldType(t reflect.Type) bool {
	return isScalarKind(underlyingKind(t))
}

// TestScalarFieldsExcludeCredentialLikeNames is a tripwire, not a coverage
// guarantee: it reflects over every scalar-typed field (see
// isScalarFieldType) of V1ProfileConfig and V1RuntimeConfig — the structs
// lookupScalarField is used against via config get's profiles/runtimes
// dotted-key support — and fails if any koanf/yaml tag matches
// credentialLikeFieldPattern. Non-scalar fields (Secrets, Env, and similar)
// are skipped: lookupScalarField already refuses them regardless of name,
// via scalarValueString, so a name match there carries no risk.
//
// No scalar field matches today. If this test starts failing, it means a
// new scalar field was added whose name matches the pattern; lookupScalarField
// already refuses it by name (see TestLookupScalarFieldRefusesCredentialLikeNames),
// so the field will not render in plaintext as-is. Treat the failure as a
// prompt to consciously decide whether refusal is the right behavior for
// that field, or to rename it — not as a sign that credential material was printed.
// This test cannot catch a credential-holding field whose name does not
// match credentialLikeFieldPattern (e.g. "passphrase"); it only checks
// names against the pattern.
func TestScalarFieldsExcludeCredentialLikeNames(t *testing.T) {
	structs := []interface{}{V1ProfileConfig{}, V1RuntimeConfig{}}
	for _, s := range structs {
		typ := reflect.TypeOf(s)
		t.Run(typ.Name(), func(t *testing.T) {
			for i := 0; i < typ.NumField(); i++ {
				f := typ.Field(i)
				if !isScalarFieldType(f.Type) {
					continue
				}
				tag := configTagName(f)
				if tag == "" {
					continue
				}
				assert.False(t, credentialLikeFieldPattern.MatchString(tag),
					"field %s.%s has scalar type and koanf/yaml tag %q, which matches the credential-like "+
						"pattern; lookupScalarField will refuse it by name via config get, so confirm that is "+
						"the intended handling for this field (or rename it) before landing", typ.Name(), f.Name, tag)
			}
		})
	}
}
