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

package maintenance

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// checkRepo is a maintenance checkout cloned from a local bare origin.
type checkRepo struct {
	t      *testing.T
	origin string
	work   string
}

func (r *checkRepo) git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *checkRepo) commit(msg string) string {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.work, "f.txt"), []byte(msg), 0o644); err != nil {
		r.t.Fatal(err)
	}
	r.git(r.work, "add", "f.txt")
	r.git(r.work, "commit", "-q", "-m", msg)
	return r.git(r.work, "rev-parse", "HEAD")
}

func newCheckRepo(t *testing.T) *checkRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
	dir := t.TempDir()
	r := &checkRepo{t: t, origin: filepath.Join(dir, "origin.git"), work: filepath.Join(dir, "work")}
	r.git(dir, "init", "-q", "--bare", "-b", "main", r.origin)
	r.git(dir, "init", "-q", "-b", "main", r.work)
	r.git(r.work, "remote", "add", "origin", r.origin)
	return r
}

// The check compares the RUNNING build's commit (when the checkout has it)
// with origin/<branch>, so a checkout that is up to date does not hide a
// running hub that is behind; otherwise it says it compared the checkout.
func TestCheckForUpdates_ComparesRunningCommit(t *testing.T) {
	ctx := context.Background()
	r := newCheckRepo(t)
	c1 := r.commit("one")
	r.git(r.work, "push", "-q", "origin", "main")
	r.commit("two")
	c3 := r.commit("three")
	r.git(r.work, "push", "-q", "origin", "main")

	check := func(running string) *UpdateCheckResult {
		t.Helper()
		res, err := CheckForUpdates(ctx, UpdateCheckOptions{RepoPath: r.work, Branch: "main", RunningCommit: running})
		if err != nil {
			t.Fatalf("CheckForUpdates(%q): %v", running, err)
		}
		return res
	}

	t.Run("running behind an up-to-date checkout", func(t *testing.T) {
		res := check(c1)
		if res.Compared != UpdateCheckRunning || res.CurrentCommit != c1 || !res.UpdateAvailable || res.CommitsBehind != 2 {
			t.Fatalf("result = %+v, want running %s two commits behind", res, c1)
		}
		if res.TrackingRef != "origin/main" || res.LatestCommit != c3 {
			t.Fatalf("result = %+v", res)
		}
		if subjects := []string{res.NewCommits[0].Subject, res.NewCommits[1].Subject}; subjects[0] != "three" || subjects[1] != "two" {
			t.Fatalf("new commits = %+v", res.NewCommits)
		}
	})

	t.Run("running the tracking commit (abbreviated id)", func(t *testing.T) {
		res := check(c3[:12])
		if res.Compared != UpdateCheckRunning || res.UpdateAvailable || res.CurrentCommit != c3 {
			t.Fatalf("result = %+v, want up to date on the running commit", res)
		}
	})

	t.Run("running ahead of the tracking ref is not behind", func(t *testing.T) {
		c4 := r.commit("local only")
		defer r.git(r.work, "reset", "-q", "--hard", c3)
		res := check(c4)
		if res.Compared != UpdateCheckRunning || res.UpdateAvailable || res.CommitsBehind != 0 {
			t.Fatalf("result = %+v, want not behind", res)
		}
	})

	t.Run("running commit not in the checkout falls back to HEAD", func(t *testing.T) {
		res := check("ffffffffffffffffffffffffffffffffffffffff")
		if res.Compared != UpdateCheckCheckout || res.CurrentCommit != c3 || res.UpdateAvailable {
			t.Fatalf("result = %+v, want the checkout compared, up to date", res)
		}
	})

	t.Run("a non-hex running value never reaches git", func(t *testing.T) {
		for _, bad := range []string{"", "unknown", "--upload-pack=touch /tmp/pwned", "HEAD~2"} {
			if res := check(bad); res.Compared != UpdateCheckCheckout {
				t.Fatalf("running %q: result = %+v, want the checkout compared", bad, res)
			}
		}
	})

	t.Run("the configured branch, not the checkout's", func(t *testing.T) {
		r.git(r.work, "checkout", "-q", "-b", "side", c1)
		r.git(r.work, "push", "-q", "origin", "side")
		defer r.git(r.work, "checkout", "-q", "main")
		res := check(c1)
		if res.TrackingRef != "origin/main" || res.CommitsBehind != 2 {
			t.Fatalf("result = %+v, want origin/main compared", res)
		}
		res, err := CheckForUpdates(ctx, UpdateCheckOptions{RepoPath: r.work, RunningCommit: c1})
		if err != nil {
			t.Fatal(err)
		}
		if res.TrackingRef != "origin/side" || res.UpdateAvailable {
			t.Fatalf("no configured branch: result = %+v, want the checkout's branch origin/side", res)
		}
	})
}
