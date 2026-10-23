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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// TestComputeContentHashMatchesTransfer guards against drift between the hub's
// content-hash adapters and the canonical transfer implementations: templates
// and harness-configs use transfer.ComputeContentHashV2 (ptone/scion#5658),
// skill versions transfer.ComputeContentHashV1, which brokers recompute to
// verify an installed skill.
func TestComputeContentHashMatchesTransfer(t *testing.T) {
	files := []store.TemplateFile{
		{Path: "home/.bashrc", Size: 12, Hash: "sha256:bbb", Mode: "0644"},
		{Path: "config.yaml", Size: 30, Hash: "sha256:aaa", Mode: "0644"},
		{Path: "scripts/run.sh", Size: 5, Hash: "sha256:ccc", Mode: "0755"},
	}

	infos := make([]transfer.FileInfo, len(files))
	for i, f := range files {
		infos[i] = transfer.FileInfo{Path: f.Path, Size: f.Size, Hash: f.Hash, Mode: f.Mode}
	}

	got := computeContentHash(files)
	want := transfer.ComputeContentHashV2(infos)
	if got != want {
		t.Fatalf("hub computeContentHash diverged from transfer.ComputeContentHashV2: got %q want %q", got, want)
	}
	if got == "" {
		t.Fatalf("expected a non-empty hash for non-empty file set")
	}
	if got, want := computeSkillContentHash(files), transfer.ComputeContentHashV1(infos); got != want {
		t.Fatalf("hub computeSkillContentHash diverged from transfer.ComputeContentHashV1: got %q want %q", got, want)
	}
}

// TestComputeContentHash_RenameAndChmod covers ptone/scion#5658 at the hub
// adapter: a rename-only or chmod-only change changes a template's or
// harness-config's content hash, but not a skill version's (v1, pinned).
func TestComputeContentHash_RenameAndChmod(t *testing.T) {
	base := []store.TemplateFile{
		{Path: "config.yaml", Hash: "sha256:aaa", Mode: "0644"},
		{Path: "run.sh", Hash: "sha256:bbb", Mode: "0644"},
	}
	// v1 concatenates file hashes in path order, so it does see a rename
	// that reorders the files; this one keeps the order.
	renamed := []store.TemplateFile{
		{Path: "config.yaml", Hash: "sha256:aaa", Mode: "0644"},
		{Path: "scripts/run.sh", Hash: "sha256:bbb", Mode: "0644"},
	}
	chmodded := []store.TemplateFile{
		{Path: "config.yaml", Hash: "sha256:aaa", Mode: "0644"},
		{Path: "run.sh", Hash: "sha256:bbb", Mode: "0755"},
	}
	for name, next := range map[string][]store.TemplateFile{"rename": renamed, "chmod": chmodded} {
		if computeContentHash(next) == computeContentHash(base) {
			t.Errorf("%s only: template content hash unchanged", name)
		}
		if computeSkillContentHash(next) != computeSkillContentHash(base) {
			t.Errorf("%s only: skill content hash changed; skill hashes must stay v1", name)
		}
	}
}

// TestComputeContentHashOrderIndependent verifies the hash is independent of the
// order files are supplied in (the canonical implementation sorts by path).
func TestComputeContentHashOrderIndependent(t *testing.T) {
	a := []store.TemplateFile{
		{Path: "a", Hash: "sha256:1"},
		{Path: "b", Hash: "sha256:2"},
		{Path: "c", Hash: "sha256:3"},
	}
	b := []store.TemplateFile{
		{Path: "c", Hash: "sha256:3"},
		{Path: "a", Hash: "sha256:1"},
		{Path: "b", Hash: "sha256:2"},
	}
	if computeContentHash(a) != computeContentHash(b) {
		t.Fatalf("content hash should be independent of input order")
	}
}

// TestComputeContentHashEmpty pins the canonical empty-input contract: an empty
// file set hashes to the empty string (matching transfer.ComputeContentHashV2),
// rather than the SHA-256 of an empty byte stream that the hub's former
// hand-rolled implementation produced.
func TestComputeContentHashEmpty(t *testing.T) {
	if got := computeContentHash(nil); got != "" {
		t.Fatalf("empty file set should hash to %q, got %q", "", got)
	}
	if got := computeContentHash([]store.TemplateFile{}); got != "" {
		t.Fatalf("empty file set should hash to %q, got %q", "", got)
	}
}
