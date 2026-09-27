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

package projectcompat

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvedPathEqual_SymlinkedPathEqualsTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "new-name", "acme")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "old-name")
	if err := os.Symlink(filepath.Join(dir, "new-name"), old); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(old, "acme")

	if !ResolvedPathEqual(oldPath, target) {
		t.Fatalf("ResolvedPathEqual(%q, %q) = false, want true", oldPath, target)
	}
	if !ResolvedPathEqual(target, oldPath) {
		t.Fatalf("ResolvedPathEqual(%q, %q) = false, want true (symmetric)", target, oldPath)
	}
}

func TestResolvedPathEqual_MissingPathFallsBackToClean(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does", "not", "exist")
	// Built by concatenation, not filepath.Join, so it is genuinely unclean
	// (a doubled separator and a trailing separator) rather than already
	// identical to missing before ResolvePathForCompare ever runs.
	unclean := dir + "//does/not/exist/"

	if !ResolvedPathEqual(missing, unclean) {
		t.Fatalf("ResolvedPathEqual(%q, %q) = false, want true (both Clean to the same path)", missing, unclean)
	}

	if ResolvedPathEqual(missing, filepath.Join(dir, "does", "not", "exist-2")) {
		t.Fatal("ResolvedPathEqual matched two different missing paths")
	}
}

func TestResolvedPathEqual_NoFalsePositivesBetweenSiblings(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "new-name", "acme")
	b := filepath.Join(dir, "new-name", "acme-2")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}

	if ResolvedPathEqual(a, b) {
		t.Fatalf("ResolvedPathEqual(%q, %q) = true, want false (siblings)", a, b)
	}
}

// TestResolvedPathEqual_RelativeAndAbsoluteMatch guards against comparing a
// relative path with an absolute path naming the same location: without
// resolving to absolute first, EvalSymlinks generally preserves relativity,
// so the two would resolve to differently-shaped strings and never compare
// equal even though they name the same file.
func TestResolvedPathEqual_RelativeAndAbsoluteMatch(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWd) }()

	if !ResolvedPathEqual("target", target) {
		t.Errorf("ResolvedPathEqual(%q, %q) = false, want true (relative path resolved against cwd)", "target", target)
	}
}

func TestResolvedPathHasPrefix(t *testing.T) {
	dir := t.TempDir()
	canonical := filepath.Join(dir, "new-configs")
	if err := os.MkdirAll(canonical, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canonical, filepath.Join(dir, "old-configs")); err != nil {
		t.Fatal(err)
	}
	realEntryDir := filepath.Join(canonical, "acme__ab12")
	if err := os.MkdirAll(realEntryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realEntryDir, ".scion"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(dir, "old-configs", "acme__ab12", ".scion")

	if !ResolvedPathHasPrefix(entry, canonical) {
		t.Fatalf("ResolvedPathHasPrefix(%q, %q) = false, want true", entry, canonical)
	}
	if ResolvedPathHasPrefix(canonical, entry) {
		t.Fatal("ResolvedPathHasPrefix reported the prefix as nested inside its own child")
	}
	if ResolvedPathHasPrefix(filepath.Join(dir, "other-dir", "x"), canonical) {
		t.Fatal("ResolvedPathHasPrefix matched an unrelated directory")
	}
	// A sibling directory whose name merely starts with the prefix's name
	// (no separator boundary) must not match — this pins the separator
	// check that guards every os.RemoveAll call built on this helper.
	evilSibling := canonical + "-evil"
	if err := os.MkdirAll(filepath.Join(evilSibling, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ResolvedPathHasPrefix(filepath.Join(evilSibling, "x"), canonical) {
		t.Fatal("ResolvedPathHasPrefix matched a sibling whose name has the prefix as a substring")
	}
}

// TestResolvedPathHasPrefix_RootPrefix guards against the filesystem root as
// the prefix: filepath.Clean leaves a trailing separator only for the root,
// so the separator-boundary check must not require a second separator after
// it, or every direct child of root would be wrongly reported as not nested
// under it.
func TestResolvedPathHasPrefix_RootPrefix(t *testing.T) {
	root := string(filepath.Separator)
	dir := t.TempDir()

	if !ResolvedPathHasPrefix(dir, root) {
		t.Fatalf("ResolvedPathHasPrefix(%q, %q) = false, want true (an absolute path nested under root is reported as such)", dir, root)
	}
	if !ResolvedPathHasPrefix(root, root) {
		t.Fatal("ResolvedPathHasPrefix(root, root) = false, want true (equal case)")
	}
}

func TestLabelValuesMatch(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "new-name", "proj")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "old-name")
	if err := os.Symlink(filepath.Join(dir, "new-name"), old); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(old, "proj")
	sibling := filepath.Join(dir, "new-name", "proj-2")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}

	if !LabelValuesMatch(LabelProjectPath, oldPath, target) {
		t.Errorf("LabelValuesMatch(%q, %q, %q) = false, want true (project path via a migrated symlink)",
			LabelProjectPath, oldPath, target)
	}
	if LabelValuesMatch(LabelProjectPath, sibling, target) {
		t.Errorf("LabelValuesMatch(%q, %q, %q) = true, want false (sibling path)",
			LabelProjectPath, sibling, target)
	}
	// A non-path label is compared exactly: a value that happens to resolve
	// to the same filesystem entity must not match unless the strings are
	// identical.
	if !LabelValuesMatch(LabelProjectID, "abc123", "abc123") {
		t.Error("LabelValuesMatch(LabelProjectID, ...) with identical values = false, want true")
	}
	if LabelValuesMatch(LabelProjectID, oldPath, target) {
		t.Error("LabelValuesMatch(LabelProjectID, ...) compared two different strings as paths, want exact string comparison")
	}
}
