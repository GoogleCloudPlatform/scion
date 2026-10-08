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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateProjectSlug_AcceptsOnlyDirectChildNames(t *testing.T) {
	accepted := []string{"my-project", "project1", "a", "proj.v2", ".hidden-name", "name."}
	for _, slug := range accepted {
		assert.NoError(t, validateProjectSlug(slug), "slug %q names a direct child and must be accepted", slug)
	}

	rejected := []string{"", ".", "..", "a/b", "a\\b", "/", "\\", "a..b", "./a", "a/", "/a", "C:foo", "c:", "a:b"}
	for _, slug := range rejected {
		assert.Error(t, validateProjectSlug(slug), "slug %q does not name a direct child and must be rejected", slug)
	}
}

func TestValidateProjectSlug_ErrorNamesTheRule(t *testing.T) {
	err := validateProjectSlug(".")
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "directly under the projects root")
	}
}

func TestIsDirectChildOfAny(t *testing.T) {
	root := filepath.Join(t.TempDir(), "projects")
	other := filepath.Join(t.TempDir(), "hub-projects")
	roots := []string{root, other}

	assert.True(t, isDirectChildOfAny(filepath.Join(root, "p1"), roots))
	assert.True(t, isDirectChildOfAny(filepath.Join(other, "p1"), roots))
	assert.True(t, isDirectChildOfAny(root+"/p1/", roots), "trailing separator cleans to a direct child")

	assert.False(t, isDirectChildOfAny(root, roots), "a root is not its own child")
	assert.False(t, isDirectChildOfAny(root+"/.", roots), "a value cleaning to the root is not a child")
	assert.False(t, isDirectChildOfAny(other+"/", roots))
	assert.False(t, isDirectChildOfAny(filepath.Join(root, "p1", "nested"), roots))
	assert.False(t, isDirectChildOfAny(root+"/p1/../..", roots))
	assert.False(t, isDirectChildOfAny(filepath.Dir(root), roots))
	assert.False(t, isDirectChildOfAny(filepath.Join(root, "p1"), nil), "no roots means nothing qualifies")
}

func TestIsDirectChildOfAny_RequiresAbsolutePaths(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "projects")

	assert.False(t, isDirectChildOfAny("p1", []string{""}), "an empty root matches nothing")
	assert.False(t, isDirectChildOfAny("projects/p1", []string{"projects"}), "a relative root matches nothing")
	assert.False(t, isDirectChildOfAny(filepath.Join(abs, "p1"), []string{"", "projects"}), "empty and relative roots match nothing")
	assert.False(t, isDirectChildOfAny("p1", []string{abs, "."}), "a relative target never matches")
	assert.False(t, isDirectChildOfAny("", []string{abs, ""}), "an empty target never matches")

	assert.True(t, isDirectChildOfAny(filepath.Join(abs, "p1"), []string{"", "projects", abs}),
		"an absolute direct child of an absolute root still matches when other roots are skipped")
}
