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

import "testing"

// TestSkillCacheKey_Golden pins skillCacheKey to the keys
// templatecache.EntryKey(hash, modes) produced before ptone/scion#5658
// moved that derivation here, so skill cache entries written by earlier
// brokers stay valid. The expected key is sha256 over
// "scion-cache-entry-v1\x00<hash>\x00x:bin/a\x00x:scripts/run.sh".
func TestSkillCacheKey_Golden(t *testing.T) {
	const hash = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	exec := ResolvedSkill{Hash: hash, Files: []ResolvedFile{
		{Path: "scripts/run.sh", Mode: "0755"},
		{Path: "SKILL.md", Mode: "0644"},
		{Path: "bin/a", Mode: "0700"},
		{Path: "legacy.txt"},
	}}
	const want = "sha256:5c0cad2bdf34281f87cfb0ab62ba208c55ce189668c2f9156a8bab0698b2c817"
	if got := skillCacheKey(exec); got != want {
		t.Errorf("skillCacheKey = %s, want %s", got, want)
	}

	plain := ResolvedSkill{Hash: hash, Files: []ResolvedFile{{Path: "SKILL.md", Mode: "0644"}, {Path: "legacy.txt"}}}
	if got := skillCacheKey(plain); got != hash {
		t.Errorf("skillCacheKey without executables = %s, want the bare bundle hash %s", got, hash)
	}
}
