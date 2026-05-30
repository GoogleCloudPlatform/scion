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
// content-hash adapter and the canonical transfer.ComputeContentHash used by
// the broker-side hydrator. If these two diverge, cache lookups silently break
// (the broker computes a different key than the hub stored), which is exactly
// the failure mode the §7.3 consolidation removes.
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
	want := transfer.ComputeContentHash(infos)
	if got != want {
		t.Fatalf("hub computeContentHash diverged from transfer.ComputeContentHash: got %q want %q", got, want)
	}
	if got == "" {
		t.Fatalf("expected a non-empty hash for non-empty file set")
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
// file set hashes to the empty string (matching transfer.ComputeContentHash),
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
