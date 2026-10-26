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

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// Broker-side verification of skill content hash v2 (ptone/scion#5935).
//
// The golden values are the ones pkg/hub's skill content hash tests pin
// (computed independently of the Go code), so the hub and the broker agree.
const (
	v2TestHashA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	v2TestHashB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	v2TestBlobA = "1111111111111111111111111111111111111111"
	v2TestBlobB = "2222222222222222222222222222222222222222"

	v2GoldenLegacyV1 = "sha256:cb6e5d6424eb09d95d48672779cf707c2500440d8775f27db011b948086bb9c2"
	v2GoldenLegacyV2 = "sha256:12d49ca2a2a621ed728f8e4721fc7f7aed53fcccddc5686dd2598b2426d186bb"
	v2GoldenGHExecV1 = "sha256:6e2183b3004ab12e5ec4bbd37d13bdd13e1f2f22c042d9f9ac4489d673529489"
	v2GoldenGHExecV2 = "sha256:87792969fc4b497e5304b83b7688dc0e3ebe12c05d47989d4cc76c201761468a"

	// The gh:// skill of pkg/hub's ghModesTestTree, as the hub resolves it.
	v2GoldenGHTreeV1 = "sha256:f8b36eda330478cbc2d7ad0c621929f05127ad7aa056a707796e950ea9e20f73"
	v2GoldenGHTreeV2 = "sha256:5901c8d81ab5c6890d31faac7e28058ed46f2325f4209c7d14b46b96713b9af6"
)

func installedInfos(files []ResolvedFile) []transfer.FileInfo {
	out := make([]transfer.FileInfo, len(files))
	for i, f := range files {
		out[i] = transfer.FileInfo{Path: f.Path, Hash: f.Hash}
	}
	return out
}

// A legacy version (no recorded modes) and a gh:// skill with an
// executable file verify against the hub's golden v1 and v2 values.
func TestVerifySkillBundleHashes_GoldenAgreesWithHub(t *testing.T) {
	legacy := []ResolvedFile{
		{Path: "SKILL.md", Hash: v2TestHashA},
		{Path: "scripts/run.sh", Hash: v2TestHashB},
	}
	gh := []ResolvedFile{
		{Path: "SKILL.md", Hash: v2TestBlobA, Mode: "0644"},
		{Path: "scripts/run.sh", Hash: v2TestBlobB, Mode: "0755"},
	}
	ghTree := []ResolvedFile{
		{Path: "SKILL.md", Hash: "ce013625030ba8dba906f756967f9e9ca394464a", Mode: "0644"},
		{Path: "scripts/run.sh", Hash: "95d09f2b10159347eece71399a7e2e907ea3df4f", Mode: "0755"},
	}
	for name, skill := range map[string]ResolvedSkill{
		"hub gh tree":      {URI: "gh://acme/hashv2-repo/tool@main", Hash: v2GoldenGHTreeV1, HashV2: v2GoldenGHTreeV2, Files: ghTree},
		"legacy, no modes": {URI: "skill://scion/core/legacy@1.0.0", Hash: v2GoldenLegacyV1, HashV2: v2GoldenLegacyV2, Files: legacy},
		"gh exec bit":      {URI: "gh://o/r/s@main", Hash: v2GoldenGHExecV1, HashV2: v2GoldenGHExecV2, Files: gh},
	} {
		if err := verifySkillBundleHashes(skill, installedInfos(skill.Files)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// v1 pins still verify (HashV2 empty, as from a hub with the experiment
// off or an older hub), v2 verifies, and each of a rename, a chmod-only
// change and a content change fails closed against v2. A wrong v1 fails
// whatever v2 says.
func TestVerifySkillBundleHashes(t *testing.T) {
	files := []ResolvedFile{
		{Path: "SKILL.md", Hash: v2TestHashA, Mode: "0644"},
		{Path: "scripts/run.sh", Hash: v2TestHashB, Mode: "0644"},
	}
	ok := ResolvedSkill{URI: "skill://scion/core/s@1.0.0", Hash: v2GoldenLegacyV1, HashV2: v2GoldenLegacyV2, Files: files}

	if err := verifySkillBundleHashes(ResolvedSkill{URI: ok.URI, Hash: ok.Hash, Files: files}, installedInfos(files)); err != nil {
		t.Errorf("v1 only: %v", err)
	}
	if err := verifySkillBundleHashes(ResolvedSkill{URI: ok.URI, HashV2: ok.HashV2, Files: files}, installedInfos(files)); err != nil {
		t.Errorf("v2 only: %v", err)
	}
	if err := verifySkillBundleHashes(ok, installedInfos(files)); err != nil {
		t.Errorf("v1 and v2: %v", err)
	}

	renamed := []ResolvedFile{files[0], {Path: "scripts/start.sh", Hash: v2TestHashB, Mode: "0644"}}
	chmodded := []ResolvedFile{files[0], {Path: "scripts/run.sh", Hash: v2TestHashB, Mode: "0755"}}
	edited := []ResolvedFile{files[0], {Path: "scripts/run.sh", Hash: v2TestHashA, Mode: "0644"}}
	for name, got := range map[string][]ResolvedFile{"rename": renamed, "chmod": chmodded, "content": edited} {
		skill := ok
		skill.Files = got
		err := verifySkillBundleHashes(ResolvedSkill{URI: ok.URI, HashV2: ok.HashV2, Files: got}, installedInfos(got))
		if err == nil || !strings.Contains(err.Error(), "bundle hash v2 mismatch") {
			t.Errorf("%s: want a v2 mismatch, got %v", name, err)
		}
		if name != "content" {
			// v1 does not see a rename or a chmod; v2 does.
			if err := verifySkillBundleHashes(ResolvedSkill{URI: ok.URI, Hash: ok.Hash, Files: got}, installedInfos(got)); err != nil {
				t.Errorf("%s: v1 alone must still pass: %v", name, err)
			}
		}
		if err := verifySkillBundleHashes(skill, installedInfos(got)); err == nil {
			t.Errorf("%s: v1+v2 must fail closed", name)
		}
	}

	badV1 := ok
	badV1.Hash = v2GoldenGHExecV1
	if err := verifySkillBundleHashes(badV1, installedInfos(files)); err == nil || !strings.Contains(err.Error(), "bundle hash mismatch") {
		t.Errorf("wrong v1: want a v1 mismatch, got %v", err)
	}
	badV2 := ok
	badV2.HashV2 = v2GoldenGHExecV2
	if err := verifySkillBundleHashes(badV2, installedInfos(files)); err == nil {
		t.Error("wrong v2 must fail closed")
	}
}

// The v2 mode comes from the resolve metadata, normalised as on the hub:
// "0700" and "0755" are the same, and a missing mode is "0644".
func TestVerifySkillBundleHashV2_ModeFromMetadata(t *testing.T) {
	files := []ResolvedFile{
		{Path: "SKILL.md", Hash: v2TestBlobA},
		{Path: "scripts/run.sh", Hash: v2TestBlobB, Mode: "0700"},
	}
	skill := ResolvedSkill{URI: "gh://o/r/s@main", HashV2: v2GoldenGHExecV2, Files: files}
	if err := verifySkillBundleHashV2(skill, installedInfos(files)); err != nil {
		t.Errorf("normalised modes: %v", err)
	}
}

// hubResolveResponseToResult carries contentHashV2 to the install step,
// and a response without it (an older hub, or the experiment off) leaves
// HashV2 empty.
func TestHubResolveResponseToResult_CarriesContentHashV2(t *testing.T) {
	const body = `{"resolved":[` +
		`{"uri":"skill://scion/core/a@1.0.0","name":"a","resolvedVersion":"1.0.0","contentHash":"sha256:v1a","contentHashV2":"sha256:v2a","files":[]},` +
		`{"uri":"skill://scion/core/b@1.0.0","name":"b","resolvedVersion":"1.0.0","contentHash":"sha256:v1b","files":[]}]}`
	var resp hubclient.ResolveSkillsResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	result := hubResolveResponseToResult([]api.SkillReference{
		{URI: "skill://scion/core/a@1.0.0"}, {URI: "skill://scion/core/b@1.0.0"},
	}, &resp)
	if len(result.Resolved) != 2 {
		t.Fatalf("resolved = %+v", result.Resolved)
	}
	if a := result.Resolved[0]; a.Hash != "sha256:v1a" || a.HashV2 != "sha256:v2a" {
		t.Errorf("a: Hash=%q HashV2=%q", a.Hash, a.HashV2)
	}
	if b := result.Resolved[1]; b.Hash != "sha256:v1b" || b.HashV2 != "" {
		t.Errorf("b: Hash=%q HashV2=%q", b.Hash, b.HashV2)
	}
}

// newV2InstallServer serves files by path and points http.DefaultTransport
// at it for the test.
func newV2InstallServer(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(content)
	}))
	t.Cleanup(srv.Close)
	origTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = origTransport })
	return srv
}

// End to end through the install step: a skill with only a v1 hash (an
// old-broker-shaped response) installs, one with matching v1 and v2
// installs, and one whose v2 was computed over a different mode, the
// rename-or-chmod case, is rejected and leaves nothing installed.
func TestInstallSkill_ContentHashV2(t *testing.T) {
	skillMD := []byte("# v2 skill\n")
	runSh := []byte("#!/bin/sh\necho hi\n")
	srv := newV2InstallServer(t, map[string][]byte{"SKILL.md": skillMD, "run.sh": runSh})

	files := []ResolvedFile{
		{Path: "SKILL.md", URL: srv.URL + "/SKILL.md", Hash: transfer.HashBytes(skillMD), Mode: "0644"},
		{Path: "scripts/run.sh", URL: srv.URL + "/run.sh", Hash: transfer.HashBytes(runSh), Mode: "0755"},
	}
	infos := []transfer.FileInfo{
		{Path: "SKILL.md", Hash: transfer.HashBytes(skillMD), Mode: "0644"},
		{Path: "scripts/run.sh", Hash: transfer.HashBytes(runSh), Mode: "0755"},
	}
	v1 := transfer.ComputeContentHashV1(infos)
	v2 := transfer.ComputeContentHashV2(infos)
	infos[1].Mode = "0644"
	v2NoExec := transfer.ComputeContentHashV2(infos)

	tests := []struct {
		name    string
		hashV2  string
		wantErr bool
	}{
		{"v1 only", "", false},
		{"v1 and v2", v2, false},
		{"v2 mismatch", v2NoExec, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skillsDest := t.TempDir()
			skill := ResolvedSkill{
				Name: "v2-skill", URI: "skill://scion/core/v2-skill@1.0.0", Version: "1.0.0",
				Hash: v1, HashV2: tt.hashV2, Files: files,
			}
			_, err := installSkillAt(context.Background(), skill, skillsDest)
			installed := filepath.Join(skillsDest, "v2-skill", "SKILL.md")
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "bundle hash v2 mismatch") {
					t.Fatalf("want a v2 mismatch, got %v", err)
				}
				if _, statErr := os.Stat(installed); !os.IsNotExist(statErr) {
					t.Errorf("a rejected skill must not be installed, stat err = %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			if _, statErr := os.Stat(installed); statErr != nil {
				t.Errorf("skill not installed: %v", statErr)
			}
		})
	}
}

// The cache-hit branch of the install step checks v2 too. A matching v2
// installs from the cache without a download; a v2 that the cached copy
// cannot satisfy (here a file listed without a per-file hash) falls
// through to the download, which verifies the downloaded files and
// installs them; a wrong v2 falls through and the download fails closed.
func TestInstallSkill_ContentHashV2CacheHit(t *testing.T) {
	content := []byte("# cached v2 skill\n")
	var downloads atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		_, _ = w.Write(content)
	}))
	t.Cleanup(srv.Close)
	origTransport := http.DefaultTransport
	http.DefaultTransport = srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = origTransport })

	fileHash := transfer.HashBytes(content)
	infos := []transfer.FileInfo{{Path: "SKILL.md", Hash: fileHash, Mode: "0644"}}
	v1 := transfer.ComputeContentHashV1(infos)
	v2 := transfer.ComputeContentHashV2(infos)
	wrongV2 := transfer.ComputeContentHashV2([]transfer.FileInfo{{Path: "README.md", Hash: fileHash, Mode: "0644"}})

	tests := []struct {
		name          string
		listedHash    string
		hashV2        string
		wantDownloads int64
		wantErr       bool
	}{
		{"v2 match uses the cache", fileHash, v2, 0, false},
		{"unverifiable cached copy falls through to download", "", v2, 1, false},
		{"wrong v2 falls through and fails closed", fileHash, wrongV2, 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			downloads.Store(0)
			cache, err := templatecache.New(t.TempDir(), 0)
			if err != nil {
				t.Fatal(err)
			}
			// No executable file, so the cache key is the bare v1 hash.
			if _, err := cache.Put(v1, map[string][]byte{"SKILL.md": content}); err != nil {
				t.Fatal(err)
			}
			ctx := ContextWithSkillCache(context.Background(), cache)
			skillsDest := t.TempDir()
			skill := ResolvedSkill{
				Name: "cached-v2", URI: "skill://scion/core/cached-v2@1.0.0", Version: "1.0.0",
				Hash: v1, HashV2: tt.hashV2,
				Files: []ResolvedFile{{Path: "SKILL.md", URL: srv.URL + "/SKILL.md", Hash: tt.listedHash, Mode: "0644"}},
			}
			_, err = installSkillAt(ctx, skill, skillsDest)
			if got := downloads.Load(); got != tt.wantDownloads {
				t.Errorf("downloads = %d, want %d", got, tt.wantDownloads)
			}
			installed := filepath.Join(skillsDest, "cached-v2", "SKILL.md")
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "bundle hash v2 mismatch") {
					t.Fatalf("want a v2 mismatch, got %v", err)
				}
				if _, statErr := os.Stat(installed); !os.IsNotExist(statErr) {
					t.Errorf("a rejected skill must not be installed, stat err = %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			got, readErr := os.ReadFile(installed)
			if readErr != nil || string(got) != string(content) {
				t.Errorf("installed SKILL.md = %q, %v", got, readErr)
			}
		})
	}
}
