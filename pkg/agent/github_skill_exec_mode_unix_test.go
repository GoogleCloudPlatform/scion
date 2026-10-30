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

//go:build !windows

package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
)

// TestGitHubSkill_ExecScriptInstalls0755 follows a gh:// skill from the
// local resolver through install (ptone/scion#5781): the executable script
// in a subdirectory lands 0755, the regular file 0644, on the first install
// and on a second one served from the skill content cache.
func TestGitHubSkill_ExecScriptInstalls0755(t *testing.T) {
	server, mux := newTestGitHubServer(t)
	var treeCalls atomic.Int64
	serveExecTestSkill(mux, &treeCalls)
	r := newTestGitHubResolver(server)

	res, err := r.Resolve(context.Background(), []api.SkillReference{{URI: "gh://owner/repo/tool@main"}}, ResolveOpts{})
	if err != nil || len(res.Errors) != 0 || len(res.Resolved) != 1 {
		t.Fatalf("Resolve: err=%v result=%+v", err, res)
	}
	skill := res.Resolved[0]

	cache, err := templatecache.New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := ContextWithSkillCache(context.Background(), cache)
	want := map[string]os.FileMode{"SKILL.md": 0o644, "scripts/run.sh": 0o755}
	for i := 0; i < 2; i++ {
		dest := t.TempDir()
		if _, err := installOneSkill(ctx, skill, "tool", dest); err != nil {
			t.Fatalf("install %d: %v", i+1, err)
		}
		assertFileModes(t, filepath.Join(dest, "tool"), want)
	}
	if _, hit := cache.Get(skillCacheKey(skill)); !hit {
		t.Fatal("skill not cached under its exec-aware key")
	}
}
