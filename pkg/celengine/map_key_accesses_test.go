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

package celengine_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
)

func TestProgram_MapKeyAccesses(t *testing.T) {
	env := celengine.Environment{
		Name:    "map_key_test",
		Version: 1,
		Variables: []celengine.Variable{
			{Name: "chain", Type: celengine.Map(celengine.String(), celengine.Dyn())},
			{Name: "key", Type: celengine.String()},
			{Name: "tags", Type: celengine.List(celengine.String())},
		},
		Extensions: []celengine.Extension{celengine.ExtensionStrings},
		Macros:     celengine.MacrosStandard,
	}
	eng, err := celengine.New(env, celengine.DefaultLimits(), celengine.DefaultCacheConfig())
	require.NoError(t, err)
	kind := celengine.Kind{Name: "filter", Result: celengine.Bool()}

	cases := []struct {
		name        string
		expr        string
		wantKeys    []string
		wantDynamic bool
	}{
		{
			name:        "no chain reference",
			expr:        `key == "hello"`,
			wantKeys:    []string{},
			wantDynamic: false,
		},
		{
			name:        "dot select nested",
			expr:        `chain.tone.flags["urgent"] == true`,
			wantKeys:    []string{"tone"},
			wantDynamic: false,
		},
		{
			name:        "has macro on chain step",
			expr:        `has(chain.tone) && chain.tone.labels["sev"] == "high"`,
			wantKeys:    []string{"tone"},
			wantDynamic: false,
		},
		{
			name:        "bracket literal index",
			expr:        `chain["step_a"].flags["ok"] == true && chain["step_b"].labels["x"] == "y"`,
			wantKeys:    []string{"step_a", "step_b"},
			wantDynamic: false,
		},
		{
			name:        "in operator with literal key",
			expr:        `"tone" in chain`,
			wantKeys:    []string{"tone"},
			wantDynamic: false,
		},
		{
			name:        "dynamic bracket index",
			expr:        `chain[key].flags["ok"] == true`,
			wantKeys:    []string{},
			wantDynamic: true,
		},
		{
			name:        "dynamic in operator",
			expr:        `key in chain`,
			wantKeys:    []string{},
			wantDynamic: true,
		},
		{
			name:        "bare chain passed to size",
			expr:        `size(chain) > 0`,
			wantKeys:    []string{},
			wantDynamic: true,
		},
		{
			name:        "bare chain equality",
			expr:        `chain == {}`,
			wantKeys:    []string{},
			wantDynamic: true,
		},
		{
			name:        "comprehension shadows chain inside loop body",
			expr:        `tags.exists(chain, chain == "x")`,
			wantKeys:    []string{},
			wantDynamic: false,
		},
		{
			name:        "comprehension shadows chain but free access outside is still detected",
			expr:        `tags.exists(chain, chain == "x") && chain.foo == true`,
			wantKeys:    []string{"foo"},
			wantDynamic: false,
		},
		{
			name:        "nested comprehension: outer binds x, inner rebinds chain",
			expr:        `tags.exists(x, tags.exists(chain, chain == x))`,
			wantKeys:    []string{},
			wantDynamic: false,
		},
		{
			name:        "nested comprehension: outer binds chain, inner binds x",
			expr:        `tags.exists(chain, tags.exists(x, chain == x))`,
			wantKeys:    []string{},
			wantDynamic: false,
		},
		{
			name:        "nested comprehension: outer binds x, inner range uses free chain.foo",
			expr:        `tags.exists(x, chain.foo.exists(chain, chain == x))`,
			wantKeys:    []string{"foo"},
			wantDynamic: false,
		},
		{
			name:        "shadowing var used in range expression (range chain is free)",
			expr:        `chain.items.exists(chain, chain == "x")`,
			wantKeys:    []string{"items"},
			wantDynamic: false,
		},
		{
			name:        "bare chain iterated in range expression is dynamic",
			expr:        `chain.exists(chain, chain == "x")`,
			wantKeys:    []string{},
			wantDynamic: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prog, err := eng.Compile(tc.expr, kind)
			require.NoError(t, err)
			keys, dyn, err := prog.MapKeyAccesses("chain")
			require.NoError(t, err)
			assert.Equal(t, tc.wantDynamic, dyn)
			assert.Equal(t, tc.wantKeys, keys)
		})
	}
}
