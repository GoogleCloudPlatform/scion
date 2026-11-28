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

package spec_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompile_ShippedAdminDefaultProbeSpec(t *testing.T) {
	fixturePath := filepath.Join("..", "..", "..", "web", "src", "components", "pages", "admin-system-one-default-probe-spec.json")
	raw, err := os.ReadFile(fixturePath)
	require.NoError(t, err, "must read the exact default probe spec shipped by admin-system-one.ts")

	compiled, err := spec.Compile(raw, spec.CompileOptions{
		Kind: spec.KindJudgmentBool,
	})
	require.NoError(t, err, "shipped admin default probe spec must compile cleanly through spec.Compile")
	require.NotNil(t, compiled)
	assert.Equal(t, spec.FormPortable, compiled.Spec.Form)
	assert.Equal(t, spec.KindJudgmentBool, compiled.Kind)
	assert.Equal(t, []string{spec.ProbeFieldText}, compiled.Inputs)
	assert.Empty(t, compiled.Spec.Binding.Instance)
	assert.Empty(t, compiled.Spec.Binding.Pool)
	require.Len(t, compiled.Spec.Questions, 1)
	assert.Equal(t, "q1", compiled.Spec.Questions[0].ID)
	assert.Equal(t, spec.PrimitiveBinary, compiled.Spec.Questions[0].Type)
	assert.NotEmpty(t, compiled.Spec.Questions[0].Instructions)
	require.NotNil(t, compiled.Spec.Output)
	assert.Equal(t, "q1", compiled.Spec.Output.Question)

	// Guard against parser permissiveness: the stale "question" field on
	// questions[0] must still fail strict syntax decoding.
	stale := bytes.Replace(raw, []byte(`"instructions"`), []byte(`"question"`), 1)
	require.NotEqual(t, raw, stale)
	_, err = spec.Compile(stale, spec.CompileOptions{
		Kind: spec.KindJudgmentBool,
	})
	require.Error(t, err)
	var ce *spec.CompileError
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, spec.CategorySyntax, ce.Category)
	assert.Contains(t, ce.Error(), `unknown field "question"`)
}
