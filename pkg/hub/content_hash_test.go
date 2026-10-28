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

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// defaultContentHash is the content hash a hub with the default
// hub.content_hash_v2 setting (the registry default, no override) records
// for files. Tests that seed or check rows without toggling the experiment
// use it, so they follow the default when ptone/scion#5995 flips it.
func defaultContentHash(files []store.TemplateFile) string {
	e, ok := experiments.Default().Lookup(experiments.ContentHashV2)
	return computeContentHashVersion(files, ok && e.Default)
}

// setContentHashV2Experiment turns hub.content_hash_v2 on or off on srv by
// replacing its registry; every other experiment keeps its default.
func setContentHashV2Experiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range srv.experimentRegistry().All() {
		if e.Name == experiments.ContentHashV2 {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.experiments = reg
	if got := srv.experimentEnabled(experiments.ContentHashV2); got != on {
		t.Fatalf("hub.content_hash_v2 = %v, want %v", got, on)
	}
}

// TestContentHashV2Experiment_RegisteredDefaultOff pins hub.content_hash_v2
// as a server-layer experiment that is off unless an operator turns it on.
func TestContentHashV2Experiment_RegisteredDefaultOff(t *testing.T) {
	e, ok := experiments.Default().Lookup(experiments.ContentHashV2)
	if !ok {
		t.Fatalf("experiment %q not registered", experiments.ContentHashV2)
	}
	if e.Default || !e.HasLayer(experiments.LayerServer) || e.Issue != "ptone/scion#5658" {
		t.Errorf("hub.content_hash_v2 metadata = %+v, want default off, server layer, issue ptone/scion#5658", e)
	}
}

// TestComputeContentHashMatchesTransfer guards against drift between the hub's
// content-hash adapters and the canonical transfer implementations: templates
// and harness-configs record transfer.ComputeContentHashV2 with
// hub.content_hash_v2 on and transfer.ComputeContentHashV1 while it is off
// (ptone/scion#5658); skill versions always record
// transfer.ComputeContentHashV1, which brokers recompute to verify an
// installed skill.
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
	v1, v2 := transfer.ComputeContentHashV1(infos), transfer.ComputeContentHashV2(infos)

	srv := &Server{}
	if got := srv.computeContentHash(files); got != v1 {
		t.Errorf("experiment off: computeContentHash = %q, want v1 %q", got, v1)
	}
	setContentHashV2Experiment(t, srv, true)
	if got := srv.computeContentHash(files); got != v2 {
		t.Errorf("experiment on: computeContentHash = %q, want v2 %q", got, v2)
	}
	setContentHashV2Experiment(t, srv, false)
	if got := srv.computeContentHash(files); got != v1 {
		t.Errorf("experiment off again: computeContentHash = %q, want v1 %q", got, v1)
	}
	var nilSrv *Server
	if got := nilSrv.computeContentHash(files); got != v1 {
		t.Errorf("nil server: computeContentHash = %q, want v1 %q", got, v1)
	}
	if got := computeSkillContentHash(files); got != v1 {
		t.Errorf("computeSkillContentHash = %q, want v1 %q", got, v1)
	}
	if got := defaultContentHash(files); got != v1 {
		t.Errorf("defaultContentHash = %q, want v1 %q while the experiment defaults off", got, v1)
	}
}

// TestComputeContentHash_RenameAndChmod covers ptone/scion#5658 at the hub
// adapter: a rename-only or chmod-only change changes the v2 content hash,
// but neither the v1 hash recorded while hub.content_hash_v2 is off nor a
// skill version's (v1, pinned).
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
		if computeContentHashVersion(next, true) == computeContentHashVersion(base, true) {
			t.Errorf("%s only: v2 content hash unchanged", name)
		}
		if computeContentHashVersion(next, false) != computeContentHashVersion(base, false) {
			t.Errorf("%s only: v1 content hash changed; v1 must stay the earlier hash", name)
		}
		if computeSkillContentHash(next) != computeSkillContentHash(base) {
			t.Errorf("%s only: skill content hash changed; skill hashes must stay v1", name)
		}
	}
}

// TestComputeContentHashOrderIndependent verifies the hash is independent of the
// order files are supplied in (the canonical implementations sort by path).
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
	for _, v2 := range []bool{false, true} {
		if computeContentHashVersion(a, v2) != computeContentHashVersion(b, v2) {
			t.Fatalf("v2=%v: content hash should be independent of input order", v2)
		}
	}
}

// TestComputeContentHashEmpty pins the canonical empty-input contract: an empty
// file set hashes to the empty string in both versions, rather than the
// SHA-256 of an empty byte stream that the hub's former hand-rolled
// implementation produced.
func TestComputeContentHashEmpty(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		if got := computeContentHashVersion(nil, v2); got != "" {
			t.Fatalf("v2=%v: empty file set should hash to %q, got %q", v2, "", got)
		}
		if got := computeContentHashVersion([]store.TemplateFile{}, v2); got != "" {
			t.Fatalf("v2=%v: empty file set should hash to %q, got %q", v2, "", got)
		}
	}
}
