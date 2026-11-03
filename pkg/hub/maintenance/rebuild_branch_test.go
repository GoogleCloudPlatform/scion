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

//go:build linux

package maintenance

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Branch-param validation in both rebuild executors (ptone/scion#6987).
// The fixture's git stub logs every call; its check-ref-format echoes the
// name back (as git does for a valid name) and it reports git 2.43.0, the
// last release whose checkout/reset do NOT accept --end-of-options.

// optionShapedBranches would be read as options by git checkout or reset.
var optionShapedBranches = []string{"--upload-pack=touch /tmp/pwned", "-b", "--orphan=x", "-"}

func TestRebuildServer_OptionShapedBranchRunsNothing(t *testing.T) {
	for _, branch := range optionShapedBranches {
		t.Run(branch, func(t *testing.T) {
			f := newRebuildFixture(t, "")
			inst, root, signals := symlinkInstallerForRebuild(t, "nightly-20261022")
			err := f.executor(inst).Run(context.Background(), &bytes.Buffer{}, map[string]string{"branch": branch})
			if err == nil || !strings.Contains(err.Error(), "cannot start with '-'") {
				t.Fatalf("Run = %v, want the branch refused", err)
			}
			if calls := f.callLog(t); calls != "" {
				t.Fatalf("commands ran for a refused branch:\n%s", calls)
			}
			if target := readLink(t, root, currentLinkName); target != filepath.Join(releasesDirName, "nightly-20261022") {
				t.Fatalf("current -> %q, want unchanged", target)
			}
			if *signals != 0 {
				t.Fatal("restart requested")
			}
		})
	}
}

func TestRebuildServer_InvalidBranchRunsNoCheckout(t *testing.T) {
	f := newRebuildFixture(t, "")
	// A check-ref-format that refuses, as git does for "a..b".
	gitStub := filepath.Join(filepath.SplitList(os.Getenv("PATH"))[0], "git")
	body := "#!/bin/sh\necho \"git $*\" >> '" + f.calls + "'\n" +
		"if [ \"$1\" = check-ref-format ]; then echo \"fatal: '$3' is not a valid branch name\" >&2; exit 128; fi\n"
	if err := os.WriteFile(gitStub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	inst, _, signals := symlinkInstallerForRebuild(t, "nightly-20261022")
	err := f.executor(inst).Run(context.Background(), &bytes.Buffer{}, map[string]string{"branch": "a..b"})
	if err == nil || err.Error() != `invalid branch "a..b": not a valid git branch name` {
		t.Fatalf("Run = %v, want the branch refused by check-ref-format", err)
	}
	if calls := f.callLog(t); strings.TrimSpace(calls) != "git check-ref-format --branch a..b" {
		t.Fatalf("calls = %q, want only the check-ref-format", calls)
	}
	if *signals != 0 {
		t.Fatal("restart requested")
	}
}

// setStubGitVersion makes the fixture's git stub report version v.
func setStubGitVersion(t *testing.T, v string) {
	t.Helper()
	gitStub := filepath.Join(filepath.SplitList(os.Getenv("PATH"))[0], "git")
	data, err := os.ReadFile(gitStub)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "git version 2.43.0") {
		t.Fatal("fixture git stub does not report 2.43.0")
	}
	if err := os.WriteFile(gitStub, []byte(strings.Replace(string(data), "git version 2.43.0", "git version "+v, 1)), 0o755); err != nil {
		t.Fatal(err)
	}
}

// On git 2.43 (Ubuntu 24.04; Debian 12 and Ubuntu 22.04 are older) checkout
// and reset read --end-of-options as a revision or path, so it is not passed.
func TestRebuildServer_Git243OmitsEndOfOptions(t *testing.T) {
	f := newRebuildFixture(t, "")
	inst, _, _ := symlinkInstallerForRebuild(t, "nightly-20261022")
	var log bytes.Buffer
	if err := f.executor(inst).Run(context.Background(), &log, map[string]string{"branch": "feature/x"}); err != nil {
		t.Fatalf("Run: %v\n%s", err, log.String())
	}
	calls := f.callLog(t)
	for _, want := range []string{
		"git check-ref-format --branch feature/x\n",
		"git checkout feature/x\n",
		"git reset --hard origin/feature/x\n",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing %q in calls:\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "--end-of-options") {
		t.Fatalf("--end-of-options passed to git 2.43:\n%s", calls)
	}
}

func TestRebuildServer_Git244UsesEndOfOptions(t *testing.T) {
	f := newRebuildFixture(t, "")
	setStubGitVersion(t, "2.44.0")
	inst, _, _ := symlinkInstallerForRebuild(t, "nightly-20261022")
	var log bytes.Buffer
	if err := f.executor(inst).Run(context.Background(), &log, map[string]string{"branch": "feature/x"}); err != nil {
		t.Fatalf("Run: %v\n%s", err, log.String())
	}
	calls := f.callLog(t)
	for _, want := range []string{
		"git checkout --end-of-options feature/x\n",
		"git reset --hard --end-of-options origin/feature/x\n",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing %q in calls:\n%s", want, calls)
		}
	}
}

func TestRebuildWeb_OptionShapedBranchRunsNothing(t *testing.T) {
	for _, branch := range optionShapedBranches {
		t.Run(branch, func(t *testing.T) {
			f := newRebuildFixture(t, "")
			e := NewRebuildWebExecutor(RebuildWebOptions{RepoPath: f.repo})
			err := e.Run(context.Background(), &bytes.Buffer{}, map[string]string{"branch": branch})
			if err == nil || !strings.Contains(err.Error(), "cannot start with '-'") {
				t.Fatalf("Run = %v, want the branch refused", err)
			}
			if calls := f.callLog(t); calls != "" {
				t.Fatalf("commands ran for a refused branch:\n%s", calls)
			}
		})
	}
}

func TestRebuildWeb_ConfiguredInvalidBranchIsRefused(t *testing.T) {
	f := newRebuildFixture(t, "")
	e := NewRebuildWebExecutor(RebuildWebOptions{RepoPath: f.repo, RepoBranch: "-c"})
	err := e.Run(context.Background(), &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot start with '-'") {
		t.Fatalf("Run = %v, want the configured branch refused", err)
	}
	if calls := f.callLog(t); calls != "" {
		t.Fatalf("commands ran for a refused branch:\n%s", calls)
	}
}

func TestRebuildWeb_EndOfOptionsByGitVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		flag    bool
	}{{"2.43.0", false}, {"2.44.0", true}} {
		t.Run(tc.version, func(t *testing.T) {
			f := newRebuildFixture(t, "")
			if tc.version != "2.43.0" {
				setStubGitVersion(t, tc.version)
			}
			e := NewRebuildWebExecutor(RebuildWebOptions{RepoPath: f.repo})
			var log bytes.Buffer
			if err := e.Run(context.Background(), &log, map[string]string{"branch": "feature/x"}); err != nil {
				t.Fatalf("Run: %v\n%s", err, log.String())
			}
			eoo := ""
			if tc.flag {
				eoo = "--end-of-options "
			}
			calls := f.callLog(t)
			for _, want := range []string{
				"git check-ref-format --branch feature/x\n",
				"git fetch origin\n",
				"git checkout " + eoo + "feature/x\n",
				"git reset --hard " + eoo + "origin/feature/x\n",
				"make web\n",
			} {
				if !strings.Contains(calls, want) {
					t.Errorf("missing %q in calls:\n%s", want, calls)
				}
			}
			if !tc.flag && strings.Contains(calls, "--end-of-options") {
				t.Fatalf("--end-of-options passed to git %s:\n%s", tc.version, calls)
			}
		})
	}
}
