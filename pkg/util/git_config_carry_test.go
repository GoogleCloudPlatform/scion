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

package util

import (
	"errors"
	"strings"
	"testing"
)

// carryCase is one value of a carried key and whether the linked policy allows it.
type carryCase struct {
	value string
	ok    bool
}

func checkCarry(t *testing.T, key string, cases []carryCase) {
	t.Helper()
	for _, c := range cases {
		v := seedConfigVar(key, c.value)
		ok, reason := allowVar(RepoPolicyLinked, v, repoConfigAllowOptions{})
		if ok != c.ok {
			t.Errorf("%s = %q: allowed %v (%q), want %v", key, c.value, ok, reason, c.ok)
		}
		if mok, _ := allowVar(RepoPolicyManaged, v, repoConfigAllowOptions{}); mok {
			t.Errorf("%s = %q: allowed under the managed policy", key, c.value)
		}
	}
}

// grep.threads is carried only as a non-negative decimal integer of at most 6 digits.
func TestAllowVar_GrepThreads(t *testing.T) {
	checkCarry(t, "grep.threads", []carryCase{
		{"0", true}, {"8", true}, {"999999", true},
		{"", false}, {"-1", false}, {"+8", false}, {"1e3", false}, {"8k", false}, {" 8", false},
		{"1234567", false}, {"0x10", false},
	})
	if ok, _ := allowVar(RepoPolicyLinked, seedConfigVar("grep.x.threads", "8"), repoConfigAllowOptions{}); ok {
		t.Error("a subsectioned grep.<x>.threads was allowed")
	}
}

// branch.<name>.vscode-merge-base is carried only as a valid ref name.
func TestAllowVar_VSCodeMergeBase(t *testing.T) {
	checkCarry(t, "branch.feature.vscode-merge-base", []carryCase{
		{"main", true}, {"origin/main", true}, {"refs/remotes/origin/main", true}, {"release-1.2", true},
		{"", false}, {"-x", false}, {"a..b", false}, {"@{u}", false}, {"@", false}, {"a b", false},
		{"a~1", false}, {"a^", false}, {"a:b", false}, {"a?", false}, {"a*", false}, {"a[", false},
		{"a\\b", false}, {"/a", false}, {"a/", false}, {"a//b", false}, {"a.", false}, {".a", false},
		{"a/.b", false}, {"a.lock", false}, {"a/b.lock/c", false}, {"a\x01b", false},
		{strings.Repeat("a", 256), false},
	})
}

// remote.<name>.promisor is carried only as a git boolean.
func TestAllowVar_RemotePromisor(t *testing.T) {
	checkCarry(t, "remote.origin.promisor", []carryCase{
		{"true", true}, {"false", true}, {"yes", true}, {"1", true},
		{"maybe", false}, {"!sh", false},
	})
}

// remote.<name>.partialclonefilter is carried only in the accepted filter forms.
func TestAllowVar_PartialCloneFilter(t *testing.T) {
	oid40 := strings.Repeat("a1", 20)
	checkCarry(t, "remote.origin.partialclonefilter", []carryCase{
		{"blob:none", true}, {"blob:limit=1m", true}, {"blob:limit=1024", true}, {"tree:0", true},
		{"object:type=blob", true}, {"sparse:oid=" + oid40, true}, {"sparse:oid=" + strings.Repeat("b", 64), true},
		{"combine:blob:none+tree:1", true}, {"combine:blob%3Anone+tree%3A1", true},
		{"", false}, {"blob:all", false}, {"blob:limit=", false}, {"blob:limit=-1", false}, {"blob:limit=1t", false},
		{"tree:", false}, {"tree:x", false}, {"object:type=file", false},
		{"sparse:path=/etc/passwd", false}, {"sparse:oid=main:.gitfilter", false}, {"sparse:oid=" + oid40[:39], false},
		{"combine:blob:none", false}, {"combine:blob:none+sparse:path=/x", false},
		{"combine:blob:none+combine%3Atree%3A1%2Btree%3A2", false}, {"combine:blob:none+tree%3", false},
		{"BLOB:NONE", false}, {"blob:none ", false}, {strings.Repeat("blob:none+", 30), false},
	})
}

// Every key of the strip list is a never-carry key, and nearby keys are not.
func TestSeedNeverCarry(t *testing.T) {
	for _, k := range []string{
		"core.editor", "core.hookspath", "core.fsmonitor", "core.pager", "core.sshcommand", "core.askpass",
		"core.gitproxy", "sequence.editor", "gpg.program", "gpg.ssh.program", "foo.program", "alias.st",
		"pager.log", "diff.tool.textconv", "diff.tool.command", "diff.external", "merge.ours.driver",
		"filter.x.clean", "filter.x.smudge", "filter.x.process", "remote.origin.uploadpack",
		"remote.origin.receivepack", "remote.origin.vcs", "uploadpack.packobjectshook",
	} {
		if !seedNeverCarry(seedConfigVar(k, "x")) {
			t.Errorf("%s is not a never-carry key", k)
		}
	}
	for _, k := range []string{
		"core.x.editor", "core.autocrlf", "sequence.x.editor", "diff.algorithm", "diff.tool.binary",
		"merge.ours.name", "filter.lfs.required", "remote.origin.url", "remote.origin.promisor",
		"grep.threads", "user.name", "uploadpack.x.packobjectshook", "pager.x.log",
	} {
		if seedNeverCarry(seedConfigVar(k, "x")) {
			t.Errorf("%s is a never-carry key", k)
		}
	}
	for name, want := range standardLFSFilter {
		if seedNeverCarry(seedConfigVar("filter.lfs."+name, want)) {
			t.Errorf("the standard git-lfs filter.lfs.%s is a never-carry key", name)
		}
		if !seedNeverCarry(seedConfigVar("filter.lfs."+name, want+" --x")) {
			t.Errorf("a non-standard filter.lfs.%s is not a never-carry key", name)
		}
	}
}

// The configuration reported in ptone/scion#7161: with stripping, core.editor and
// core.hookspath are left out of the copy and named; the four carried keys are
// copied with their values unchanged; nothing is refused. Without stripping the
// two never-carry keys are refused.
func TestPrepareSeededGitConfig_ReportedConfig(t *testing.T) {
	raw := "[core]\n\teditor = true\n\thooksPath = .githooks\n" +
		"[grep]\n\tthreads = 8\n" +
		"[branch \"feature\"]\n\tvscode-merge-base = origin/main\n" +
		"[remote \"origin\"]\n\turl = https://example.com/r.git\n\tpromisor = true\n\tpartialclonefilter = blob:none\n"
	out, _, never, err := PrepareSeededGitConfig([]byte(raw), RepoPolicyLinked, nil, true)
	if err != nil {
		t.Fatalf("refused with stripping: %v", err)
	}
	if got := strings.Join(never, ","); got != "core.editor,core.hookspath" {
		t.Errorf("left out %q, want core.editor,core.hookspath", got)
	}
	vars, err := parseGitConfigFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range vars {
		got[v.Key()] = v.Value
	}
	for k, want := range map[string]string{
		"grep.threads": "8", "branch.feature.vscode-merge-base": "origin/main",
		"remote.origin.promisor": "true", "remote.origin.partialclonefilter": "blob:none",
		"remote.origin.url": "https://example.com/r.git",
	} {
		if got[k] != want {
			t.Errorf("copy has %s = %q, want %q", k, got[k], want)
		}
	}
	for _, k := range []string{"core.editor", "core.hookspath"} {
		if _, ok := got[k]; ok {
			t.Errorf("copy still has %s", k)
		}
	}

	_, _, _, err = PrepareSeededGitConfig([]byte(raw), RepoPolicyLinked, nil, false)
	var many *RepoConfigRefusals
	if !errors.As(err, &many) || len(many.Refusals) != 2 || many.Refusals[0].Key != "core.editor" ||
		many.Refusals[1].Key != "core.hookspath" || many.Refusals[0].Reason != neverCarryReason {
		t.Fatalf("without stripping: %v, want core.editor and core.hookspath refused as never-carry", err)
	}
}

// Stripping leaves out only never-carry keys: an unknown key and an include
// still refuse, and a refused copy is never produced.
func TestPrepareSeededGitConfig_StripRefusesOthers(t *testing.T) {
	raw := "[core]\n\teditor = vi\n[mytool]\n\tsetting = 1\n[include]\n\tpath = /tmp/x\n[url \"https://evil/\"]\n\tinsteadOf = https://example.com/\n"
	out, _, never, err := PrepareSeededGitConfig([]byte(raw), RepoPolicyLinked, nil, true)
	var many *RepoConfigRefusals
	if !errors.As(err, &many) || out != nil || never != nil {
		t.Fatalf("got out %q never %v err %v; want a refusal and no copy", out, never, err)
	}
	var keys []string
	for _, r := range many.Refusals {
		keys = append(keys, r.Key)
	}
	if got := strings.Join(keys, ","); got != "mytool.setting,include.path,url.https://evil/.insteadof" {
		t.Errorf("refused %q, want the unknown key, the include and the redirect (not core.editor)", got)
	}
}

// The notice names the left-out keys and stays within the stored-message bound.
func TestNeverCarriedNotice(t *testing.T) {
	if NeverCarriedNotice(nil) != "" {
		t.Error("notice for no keys")
	}
	want := "agent git config: left out core.editor, core.hookspath from the agent's copy of .git/config; settings that name a command or a hook location are never given to agents"
	if got := NeverCarriedNotice([]string{"core.editor", "core.hookspath"}); got != want {
		t.Errorf("notice = %q, want %q", got, want)
	}
	var many []string
	for i := 0; i < 60; i++ {
		many = append(many, "alias.a-long-alias-name-"+strings.Repeat("x", i%7))
	}
	got := NeverCarriedNotice(many)
	if len(got) > maxRefusalsTextBytes || !strings.Contains(got, " more from the agent's copy") || !strings.HasPrefix(got, "agent git config: left out alias.") {
		t.Errorf("long notice (%d bytes) = %q", len(got), got)
	}
	long := NeverCarriedNotice([]string{"alias." + strings.Repeat("y", 900)})
	if len(long) > maxRefusalsTextBytes || !strings.Contains(long, "… from the agent's copy") {
		t.Errorf("one long name (%d bytes) = %q", len(long), long)
	}
}
