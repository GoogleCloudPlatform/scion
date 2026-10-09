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

// Tests for the encoding/json embedding rules structJSONNames follows
// (ptone/scion#3898).

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type jnDeep struct {
	Deep   string `json:"deep"`
	Shared string `json:"shared"` // depth 2: loses to jnLevel1.Shared
}

type jnLevel1 struct {
	jnDeep
	Mid    string `json:"mid"`    // depth 1: loses to jnRoot.Over
	Shared string `json:"shared"` // depth 1: wins over jnDeep.Shared
}

type jnConflictA struct {
	Dup      string `json:"dup"`    // tagged at equal depth in both: dropped
	X        string `json:"TagWin"` // the only tagged TagWin: wins
	Untagged string // untagged at equal depth in both: dropped
}

// JNConflictB is exported and embedded through a pointer so that its Dup
// tag repeats jnConflictA's at equal depth for encoding/json without go
// vet's structtag check (which does not follow embedded pointers)
// reporting the deliberate duplicate.
type JNConflictB struct {
	Dup      string `json:"dup"`
	TagWin   string
	Untagged string
}

// JNPtr is exported so that the embedded pointer to it is walked.
type JNPtr struct {
	Ptr string `json:"ptr"`
}

type jnTaggedEmbed struct {
	Inner string `json:"inner"`
}

type jnHidden struct {
	Hidden string `json:"hidden"`
}

type jnRoot struct {
	jnLevel1
	jnConflictA
	*JNConflictB
	*JNPtr
	jnTaggedEmbed `json:"tagged_embed"` // a tagged embedded struct is a named field
	*jnHidden                           // unexported embedded pointer: skipped (see structJSONNames)
	Over          string                `json:"mid"` // depth 0: wins over jnLevel1.Mid
}

func TestStructJSONNames_EmbeddingRules(t *testing.T) {
	typ := reflect.TypeOf(jnRoot{})
	for _, tc := range []struct {
		key       string
		wantName  string // "" means no match
		wantIndex []int
	}{
		{"deep", "Deep", []int{0, 0, 0}},  // promoted through two levels
		{"shared", "Shared", []int{0, 2}}, // the shallowest field wins
		{"mid", "Over", []int{6}},         // a direct field beats a promoted one
		{"dup", "", nil},                  // tagged at equal depth twice: dropped
		{"Untagged", "", nil},             // untagged at equal depth twice: dropped
		{"TagWin", "X", []int{1, 1}},      // the only tagged one wins at equal depth
		{"ptr", "Ptr", []int{3, 0}},       // promoted through an embedded pointer
		{"tagged_embed", "jnTaggedEmbed", []int{4}},
		{"inner", "", nil},                // a tagged embedded struct is not flattened
		{"hidden", "", nil},               // unexported embedded pointer: skipped
		{"SHARED", "Shared", []int{0, 2}}, // case-insensitive match uses the winner
	} {
		t.Run(tc.key, func(t *testing.T) {
			f, ok := structFieldByJSONName(typ, tc.key)
			if tc.wantName == "" {
				assert.False(t, ok, "matched %s", f.Name)
				return
			}
			require.True(t, ok)
			assert.Equal(t, tc.wantName, f.Name)
			assert.Equal(t, tc.wantIndex, f.Index)
		})
	}
}

// The names structJSONNames yields are the keys encoding/json writes for
// the same type (with the exported embedded pointers set and the
// unexported one nil, which encoding/json leaves out).
func TestStructJSONNames_MatchesEncodingJSON(t *testing.T) {
	v := jnRoot{JNConflictB: &JNConflictB{}, JNPtr: &JNPtr{Ptr: "p"}}
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &m))
	var want []string
	for k := range m {
		want = append(want, k)
	}
	sort.Strings(want)

	var got []string
	for name := range structJSONNames(reflect.TypeOf(jnRoot{})) {
		got = append(got, name)
	}
	sort.Strings(got)
	assert.Equal(t, want, got)
}

// mergeSettingsStruct writes a promoted field at the outer YAML level,
// which matches yaml.v3 only when the embedded struct is tagged
// yaml:",inline". No type reachable from the server config may embed a
// struct without it.
func TestV1ServerConfig_EmbeddedStructsAreYAMLInline(t *testing.T) {
	configPkg := reflect.TypeOf(config.V1ServerConfig{}).PkgPath()
	seen := map[reflect.Type]bool{}
	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		// Only the scion config types are checked; the merge treats
		// types from other packages as opaque values in practice.
		if typ.Kind() != reflect.Struct || seen[typ] || typ.PkgPath() != configPkg {
			return
		}
		seen[typ] = true
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if f.Anonymous && ft.Kind() == reflect.Struct {
				opts := strings.Split(f.Tag.Get("yaml"), ",")[1:]
				inline := false
				for _, o := range opts {
					inline = inline || o == "inline"
				}
				assert.True(t, inline, "%s embeds %s without yaml:\",inline\"", path, ft)
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	walk(reflect.TypeOf(config.V1ServerConfig{}), "V1ServerConfig")
}
