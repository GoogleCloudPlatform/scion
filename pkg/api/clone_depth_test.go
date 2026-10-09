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
	"strconv"
	"strings"
	"testing"
	"time"

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
		// The value is capped at MaxCloneDepth (9 digits), like the
		// schemas, so it always fits an int.
		{in: "999999999", depth: 999999999, ok: true},
		{in: "1000000000", wantErr: true},
		{in: "9223372036854775808", wantErr: true},
		{in: "99999999999999999999", wantErr: true},
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
		// A number is read as a float64, as the schema validator reads
		// it, so digits beyond float64 precision are dropped.
		{name: "beyond float64 precision", json: `{"clone_depth":5.0000000000000001}`, yaml: "clone_depth: 5.0000000000000001\n", want: "5"},
		// Values above the cap keep a form GitDepth rejects.
		{name: "above int64", json: `{"clone_depth":9223372036854775808}`, yaml: "clone_depth: 9223372036854775808\n", want: "9223372036854775808"},
		{name: "large exponent", json: `{"clone_depth":1e19}`, yaml: "clone_depth: 1e19\n", want: "1e+19"},
		// A bool is kept as text (and rejected).
		{name: "bool", json: `{"clone_depth":true}`, yaml: "clone_depth: true\n", want: "true"},
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
	assert.Error(t, json.Unmarshal([]byte(`{"clone_depth":[1]}`), &w))
	assert.Error(t, json.Unmarshal([]byte(`{"clone_depth":{"a":1}}`), &w))
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
		{in: ".inf", want: "+Inf"},
		{in: ".nan", want: "NaN"},
		{in: "true", want: "true"},
		// A tagged string is kept as decoded, not as written.
		{in: "!!binary NQ==", want: "5"},
		{in: "!!str 7", want: "7"},
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

// JSON numbers that do not fit a float64 decode as yaml.v3 decodes them
// (as text), without expanding them, and GitDepth rejects them.
func TestCloneDepth_DecodeJSONHugeNumbers(t *testing.T) {
	var d CloneDepth
	require.NoError(t, json.Unmarshal([]byte(`1e1000000000`), &d))
	assert.Equal(t, CloneDepth("1e1000000000"), d)
	assert.Error(t, d.Validate())

	million := "1" + strings.Repeat("0", 1000000)
	require.NoError(t, json.Unmarshal([]byte(million), &d))
	assert.Equal(t, CloneDepth(million), d)
	assert.Error(t, d.Validate())

	require.NoError(t, json.Unmarshal([]byte(`1e64`), &d))
	assert.Equal(t, CloneDepth("1e+64"), d)
	assert.Error(t, d.Validate())
}

// Decoding a long digit run takes time linear in its length, both for a
// bare integer and for a long mantissa with a small exponent. Each case
// is a 1e6-digit number that must decode, and be rejected, well under a
// second.
func TestCloneDepth_BoundedDecodeCostForLongNumbers(t *testing.T) {
	digits := "1" + strings.Repeat("0", 1000000)
	for _, tt := range []struct{ name, lit string }{
		{name: "integer", lit: digits},
		{name: "mantissa with exponent", lit: digits + "e-64"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			var d CloneDepth
			require.NoError(t, json.Unmarshal([]byte(tt.lit), &d))
			var y struct {
				D CloneDepth `yaml:"clone_depth"`
			}
			require.NoError(t, yaml.Unmarshal([]byte("clone_depth: "+tt.lit), &y))
			elapsed := time.Since(start)
			assert.Error(t, d.Validate())
			assert.Error(t, y.D.Validate())
			assert.Less(t, elapsed, 400*time.Millisecond, "decoding took %s", elapsed)
		})
	}
}

func TestCloneDepthFromValue(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   interface{}
		want CloneDepth
	}{
		{name: "nil", in: nil, want: ""},
		{name: "string", in: "full", want: "full"},
		{name: "int", in: 7, want: "7"},
		{name: "int64", in: int64(7), want: "7"},
		{name: "uint64", in: uint64(18446744073709551615), want: "18446744073709551615"},
		{name: "whole float", in: 7.0, want: "7"},
		{name: "fraction", in: 7.5, want: "7.5"},
		{name: "huge float", in: 1e300, want: "1e+300"},
		{name: "bool", in: true, want: "true"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CloneDepthFromValue(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
	_, err := CloneDepthFromValue(map[string]interface{}{"a": 1})
	assert.Error(t, err)
	_, err = CloneDepthFromValue([]interface{}{1})
	assert.Error(t, err)
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

// maxCloneDepthDigits stays in step with MaxCloneDepth.
func TestMaxCloneDepthDigits(t *testing.T) {
	if got := len(strconv.Itoa(MaxCloneDepth)); got != maxCloneDepthDigits {
		t.Errorf("MaxCloneDepth has %d digits, maxCloneDepthDigits is %d", got, maxCloneDepthDigits)
	}
}
