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

package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestCloneDepth_GitDepth(t *testing.T) {
	tests := []struct {
		in      CloneDepth
		depth   int
		ok      bool
		wantErr bool
	}{
		{in: "", ok: false},
		{in: "full", depth: 0, ok: true},
		{in: "FULL", wantErr: true},
		{in: "Full", wantErr: true},
		{in: " full ", wantErr: true},
		{in: " 5 ", wantErr: true},
		{in: "+5", wantErr: true},
		{in: "007", wantErr: true},
		{in: "99999999999999999999999", wantErr: true},
		{in: "1", depth: 1, ok: true},
		{in: "50", depth: 50, ok: true},
		{in: "0", wantErr: true},
		{in: "-3", wantErr: true},
		{in: "shallow", wantErr: true},
		{in: "1.5", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.in), func(t *testing.T) {
			depth, ok, err := tt.in.GitDepth()
			if tt.wantErr {
				require.Error(t, err)
				assert.Error(t, tt.in.Validate())
				return
			}
			require.NoError(t, err)
			assert.NoError(t, tt.in.Validate())
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.depth, depth)
		})
	}
}

func TestCloneDepth_DecodeIntegerOrString(t *testing.T) {
	type wrapper struct {
		CloneDepth CloneDepth `json:"clone_depth,omitempty" yaml:"clone_depth,omitempty"`
	}
	for _, tt := range []struct {
		name, json, yaml string
		want             CloneDepth
	}{
		{name: "full", json: `{"clone_depth":"full"}`, yaml: "clone_depth: full\n", want: "full"},
		{name: "integer", json: `{"clone_depth":50}`, yaml: "clone_depth: 50\n", want: "50"},
		{name: "quoted integer", json: `{"clone_depth":"50"}`, yaml: "clone_depth: \"50\"\n", want: "50"},
		{name: "unset", json: `{}`, yaml: "{}\n", want: ""},
		{name: "null", json: `{"clone_depth":null}`, yaml: "clone_depth: null\n", want: ""},
		// Whole-valued numbers in other notations become decimal, because
		// the schemas accept them as integers.
		{name: "whole float", json: `{"clone_depth":5.0}`, yaml: "clone_depth: 5.0\n", want: "5"},
		{name: "exponent", json: `{"clone_depth":1e2}`, yaml: "clone_depth: 1e2\n", want: "100"},
		{name: "exponent with fraction", json: `{"clone_depth":2.5e1}`, yaml: "clone_depth: 2.5e1\n", want: "25"},
		// Fractions and quoted text are kept as written (and rejected).
		{name: "fraction", json: `{"clone_depth":1.5}`, yaml: "clone_depth: 1.5\n", want: "1.5"},
		{name: "negative", json: `{"clone_depth":-3}`, yaml: "clone_depth: -3\n", want: "-3"},
		{name: "quoted float", json: `{"clone_depth":"5.0"}`, yaml: "clone_depth: \"5.0\"\n", want: "5.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var j wrapper
			require.NoError(t, json.Unmarshal([]byte(tt.json), &j))
			assert.Equal(t, tt.want, j.CloneDepth)
			var y wrapper
			require.NoError(t, yaml.Unmarshal([]byte(tt.yaml), &y))
			assert.Equal(t, tt.want, y.CloneDepth)
		})
	}

	var w wrapper
	assert.Error(t, json.Unmarshal([]byte(`{"clone_depth":true}`), &w))
	assert.Error(t, yaml.Unmarshal([]byte("clone_depth: [1]\n"), &w))
	assert.Error(t, yaml.Unmarshal([]byte("clone_depth: {a: 1}\n"), &w))
}

// YAML-only integer notations decode to the value yaml.v3 gives an
// untyped decode (what the schema validator checks).
func TestCloneDepth_DecodeYAMLIntegerNotations(t *testing.T) {
	type wrapper struct {
		CloneDepth CloneDepth `yaml:"clone_depth"`
	}
	for _, tt := range []struct {
		in   string
		want CloneDepth
	}{
		{in: "0x10", want: "16"},
		{in: "0o20", want: "16"},
		{in: "+5", want: "5"},
		{in: ".inf", want: ".inf"},
		{in: ".nan", want: ".nan"},
		{in: "true", want: "true"},
	} {
		t.Run(tt.in, func(t *testing.T) {
			var y wrapper
			require.NoError(t, yaml.Unmarshal([]byte("clone_depth: "+tt.in+"\n"), &y))
			assert.Equal(t, tt.want, y.CloneDepth)
		})
	}

	// An alias to a scalar resolves to that scalar.
	var y struct {
		A int        `yaml:"a"`
		D CloneDepth `yaml:"clone_depth"`
	}
	require.NoError(t, yaml.Unmarshal([]byte("a: &n 0x10\nclone_depth: *n\n"), &y))
	assert.Equal(t, CloneDepth("16"), y.D)
}

// A JSON number with a huge exponent is kept as written instead of being
// expanded, and GitDepth rejects it.
func TestCloneDepth_DecodeJSONHugeExponent(t *testing.T) {
	var d CloneDepth
	require.NoError(t, json.Unmarshal([]byte(`1e1000000000`), &d))
	assert.Equal(t, CloneDepth("1e1000000000"), d)
	assert.Error(t, d.Validate())

	require.NoError(t, json.Unmarshal([]byte(`1e64`), &d))
	assert.Equal(t, CloneDepth("1"+strings.Repeat("0", 64)), d)
	assert.Error(t, d.Validate())
}

func TestScionConfig_CloneDepthJSONRoundTrip(t *testing.T) {
	in := ScionConfig{CloneDepth: "25"}
	b, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"clone_depth":"25"`)
	var out ScionConfig
	require.NoError(t, json.Unmarshal(b, &out))
	assert.Equal(t, CloneDepth("25"), out.CloneDepth)

	b, err = json.Marshal(ScionConfig{})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "clone_depth")
}
