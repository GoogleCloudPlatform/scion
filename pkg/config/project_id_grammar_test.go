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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// malformedProjectIDs lists values that do not match the project ID format.
var malformedProjectIDs = []struct {
	name string
	id   string
}{
	{"empty", ""},
	{"whitespace only", "   "},
	{"dot", "."},
	{"dot dot", ".."},
	{"forward slash", "a/b"},
	{"leading dot dot element", "../abcdef12"},
	{"nested dot dot elements", "../../outside"},
	{"leading slash", "/../../x"},
	{"backslash", `a\b`},
	{"nul byte", "abc\x00def"},
	{"leading dash", "-abc"},
	{"leading dot", ".abc"},
	{"inner space", "abc def"},
	{"colon", "abc:def"},
	{"too long", strings.Repeat("a", 129)},
}

// validProjectIDs lists values produced by GenerateProjectID or used by
// existing projects and fixtures.
var validProjectIDs = []string{
	GenerateProjectID(),
	"550e8400-e29b-41d4-a716-446655440000",
	"550E8400-E29B-41D4-A716-446655440000",
	"short",
	"12345678",
	"abc-123",
	"local-deterministic-id",
	"550e8400-e29b-41d4-a716-446655440000__my-project",
	strings.Repeat("a", 128),
}

func TestReadProjectID_RejectsMalformedValues(t *testing.T) {
	for _, tt := range malformedProjectIDs {
		t.Run(tt.name, func(t *testing.T) {
			scionDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(scionDir, "project-id"), []byte(tt.id+"\n"), 0644); err != nil {
				t.Fatal(err)
			}
			got, err := ReadProjectID(scionDir)
			if err == nil {
				t.Fatalf("ReadProjectID() = %q, nil; want an error", got)
			}
			if os.IsNotExist(err) {
				t.Fatalf("ReadProjectID() error = %v; must not report a missing file", err)
			}
			if !errors.Is(err, ErrInvalidProjectID) {
				t.Errorf("ReadProjectID() error = %v; want ErrInvalidProjectID", err)
			}
		})
	}
}

func TestValidateProjectID(t *testing.T) {
	for _, tt := range malformedProjectIDs {
		if err := ValidateProjectID(tt.id); !errors.Is(err, ErrInvalidProjectID) {
			t.Errorf("ValidateProjectID(%q) = %v; want ErrInvalidProjectID", tt.id, err)
		}
	}
	for _, id := range validProjectIDs {
		if err := ValidateProjectID(id); err != nil {
			t.Errorf("ValidateProjectID(%q) = %v; want nil", id, err)
		}
	}
}

func TestReadProjectMarker_RejectsMalformedProjectID(t *testing.T) {
	for _, tt := range malformedProjectIDs {
		if strings.TrimSpace(tt.id) == "" {
			continue // covered by the missing-field check
		}
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".scion")
			if err := WriteProjectMarker(path, &ProjectMarker{
				ProjectID:   tt.id,
				ProjectName: "proj",
				ProjectSlug: "proj",
			}); err != nil {
				t.Fatal(err)
			}
			if m, err := ReadProjectMarker(path); err == nil {
				t.Fatalf("ReadProjectMarker() project-id = %q, nil; want an error", m.ProjectID)
			}
		})
	}
}

func TestReadProjectID_ValidValuesRoundTrip(t *testing.T) {
	for _, id := range validProjectIDs {
		t.Run(id, func(t *testing.T) {
			scionDir := t.TempDir()
			if err := WriteProjectID(scionDir, id); err != nil {
				t.Fatal(err)
			}
			got, err := ReadProjectID(scionDir)
			if err != nil {
				t.Fatalf("ReadProjectID() error: %v", err)
			}
			if got != id {
				t.Errorf("ReadProjectID() = %q, want %q", got, id)
			}
		})
	}
}

func TestReadProjectMarker_ValidValuesRoundTrip(t *testing.T) {
	for _, id := range validProjectIDs {
		t.Run(id, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".scion")
			want := &ProjectMarker{ProjectID: id, ProjectName: "My Project", ProjectSlug: "my-project"}
			if err := WriteProjectMarker(path, want); err != nil {
				t.Fatal(err)
			}
			got, err := ReadProjectMarker(path)
			if err != nil {
				t.Fatalf("ReadProjectMarker() error: %v", err)
			}
			if *got != *want {
				t.Errorf("ReadProjectMarker() = %+v, want %+v", *got, *want)
			}
		})
	}
}

func TestProjectMarker_ValidIDsKeepDirName(t *testing.T) {
	tests := []struct {
		id, slug, want string
	}{
		{"550e8400-e29b-41d4-a716-446655440000", "my-project", "my-project__550e8400"},
		{"550E8400-E29B-41D4-A716-446655440000", "my-project", "my-project__550E8400"},
		{"short", "test", "test__short"},
		{"abc-123", "test", "test__abc123"},
		{"12345678", "test", "test__12345678"},
	}
	for _, tt := range tests {
		m := ProjectMarker{ProjectID: tt.id, ProjectSlug: tt.slug}
		if got := m.DirName(); got != tt.want {
			t.Errorf("DirName(%q, %q) = %q, want %q", tt.id, tt.slug, got, tt.want)
		}
	}
}

// assertSingleElement fails unless name is one non-special path element.
func assertSingleElement(t *testing.T, what, name string) {
	t.Helper()
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\\x00") || !filepath.IsLocal(name) {
		t.Errorf("%s = %q, want a single path element", what, name)
	}
}

func TestProjectMarker_ShortUUIDAndDirNameAreSingleElement(t *testing.T) {
	ids := []string{}
	for _, tt := range malformedProjectIDs {
		ids = append(ids, tt.id)
	}
	ids = append(ids, validProjectIDs...)
	ids = append(ids, "/", `\`, "a/../../b", "--------/x", "./", "........")

	slugs := []string{"my-project", "", ".", "..", "a/b", "../..", `a\b`, "x\x00y", "/"}

	parent := filepath.Join(t.TempDir(), "project-configs")
	for _, id := range ids {
		for _, slug := range slugs {
			m := ProjectMarker{ProjectID: id, ProjectSlug: slug}
			short := m.ShortUUID()
			assertSingleElement(t, "ShortUUID("+id+")", short)

			dir := m.DirName()
			assertSingleElement(t, "DirName("+id+", "+slug+")", dir)
			if got := filepath.Dir(filepath.Join(parent, dir)); got != parent {
				t.Errorf("DirName(%q, %q) = %q resolves under %q, want %q", id, slug, dir, got, parent)
			}
		}
	}
}

func TestProjectMarker_ExternalProjectPathUnderProjectConfigs(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	parent := filepath.Join(tmpHome, GlobalDir, ProjectConfigsDir)

	for _, tt := range malformedProjectIDs {
		m := ProjectMarker{ProjectID: tt.id, ProjectSlug: "proj"}
		got, err := m.ExternalProjectPath()
		if err == nil {
			t.Errorf("ExternalProjectPath(%q) = %q, nil; want an error", tt.id, got)
		}
	}
	for _, id := range validProjectIDs {
		m := ProjectMarker{ProjectID: id, ProjectSlug: "proj"}
		got, err := m.ExternalProjectPath()
		if err != nil {
			t.Errorf("ExternalProjectPath(%q) error: %v", id, err)
			continue
		}
		if filepath.Dir(filepath.Dir(got)) != parent {
			t.Errorf("ExternalProjectPath(%q) = %q, want a child of %q", id, got, parent)
		}
	}
}

func TestInitInRepoProject_CreatesExternalDirsUnderProjectConfigs(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectDir := filepath.Join(t.TempDir(), "my-repo", ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := WriteProjectID(projectDir, "550e8400-e29b-41d4-a716-446655440000"); err != nil {
		t.Fatal(err)
	}

	if err := initInRepoProject(projectDir, InitProjectOpts{SkipRuntimeCheck: true}); err != nil {
		t.Fatalf("initInRepoProject() error: %v", err)
	}

	want := filepath.Join(tmpHome, GlobalDir, ProjectConfigsDir, "my-repo__550e8400", DotScion, "agents")
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("expected external agents dir at %s: %v", want, err)
	}
}

func TestInitInRepoProject_RequiresWellFormedProjectID(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectDir := filepath.Join(t.TempDir(), "my-repo", ".scion")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "project-id"), []byte("/../../x\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := initInRepoProject(projectDir, InitProjectOpts{SkipRuntimeCheck: true}); err == nil {
		t.Error("initInRepoProject() = nil; want an error for a malformed project-id")
	}

	// Nothing may be created in ~/.scion other than the project-configs tree.
	entries, err := os.ReadDir(filepath.Join(tmpHome, GlobalDir))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != ProjectConfigsDir {
			t.Errorf("unexpected entry %q created in %s", e.Name(), filepath.Join(tmpHome, GlobalDir))
		}
	}
}
