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
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// seedTestStrip is the shape of the agent seed's strip predicate: the stale
// upstream of branch agent-x and push.autoSetupRemote.
func seedTestStrip(section, subsection string, hasSubsection bool, name string) bool {
	if section == "push" && !hasSubsection && name == "autosetupremote" {
		return true
	}
	return section == "branch" && hasSubsection && subsection == "agent-x"
}

// seedConfigVar builds a parsed variable from a dotted key, as allowVar sees it.
func seedConfigVar(key, value string) gitConfigVar {
	sec, sub, name := splitConfigKey(key)
	return gitConfigVar{Section: strings.ToLower(sec), Subsection: sub, HasSubsection: sub != "", Name: strings.ToLower(name), Value: value}
}

// mustRefuseSeed runs PrepareSeededGitConfig on raw, requires a seed refusal
// naming wantKey, and requires that neither the error text nor the key carries
// marker (a distinctive substring of the refused value).
func mustRefuseSeed(t *testing.T, raw, wantKey, marker string) *RepoConfigRefusedError {
	t.Helper()
	out, _, err := PrepareSeededGitConfig([]byte(raw), RepoPolicyLinked, seedTestStrip)
	if err == nil {
		t.Fatalf("seed accepted a base setting %s; output:\n%s", wantKey, out)
	}
	if out != nil {
		t.Errorf("a refused seed returned output:\n%s", out)
	}
	var refused *RepoConfigRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("error is %T (%v), want *RepoConfigRefusedError", err, err)
	}
	if refused.Key != wantKey {
		t.Errorf("refused key = %q, want %q", refused.Key, wantKey)
	}
	if !refused.Seeded {
		t.Error("seed refusal is not marked Seeded")
	}
	msg := err.Error()
	if !strings.Contains(msg, wantKey) {
		t.Errorf("error does not name the key %q: %s", wantKey, msg)
	}
	if !strings.Contains(msg, "remove it from the repository's .git/config") {
		t.Errorf("error does not carry the seed remedy: %s", msg)
	}
	// The agent never reads the user's global git config, so the remedy must
	// not point there.
	if strings.Contains(msg, "global") {
		t.Errorf("seed remedy points at a global git config: %s", msg)
	}
	if marker != "" {
		if !strings.Contains(raw, marker) {
			t.Fatalf("marker %q is not in the base, so its absence proves nothing", marker)
		}
		if strings.Contains(msg, marker) {
			t.Errorf("error leaks the refused value (marker %q): %s", marker, msg)
		}
		if strings.Contains(refused.Key, marker) {
			t.Errorf("refused key leaks marker %q: %s", marker, refused.Key)
		}
	}
	return refused
}

func TestPrepareSeededGitConfig_RefusesCommandKeyWithoutValue(t *testing.T) {
	raw := "[core]\n\trepositoryformatversion = 0\n\tsshCommand = sshpass -p seedmarker-q7v2 ssh\n"
	_ = mustRefuseSeed(t, raw, "core.sshcommand", "seedmarker-q7v2")
}

// The host-side refusal keeps its own wording; only a seed refusal names the
// per-layer remedy (and never a global config).
func TestRepoConfigRefusedError_HostWordingUnchanged(t *testing.T) {
	host := (&RepoConfigRefusedError{Dir: "/r", Key: "core.sshcommand", Value: "v-marker", Reason: "x"}).Error()
	want := "git in /r refused: repository config sets core.sshcommand, which is x; remove it from the repository's .git/config (or config.worktree)"
	if host != want {
		t.Errorf("host wording = %q, want %q", host, want)
	}
	seed := (&RepoConfigRefusedError{Key: "core.sshcommand", Value: "v-marker", Reason: "x", Seeded: true}).Error()
	if strings.Contains(seed, "v-marker") || strings.Contains(host, "v-marker") {
		t.Errorf("Error() printed the value: host %q, seed %q", host, seed)
	}
}

// A base written by earlier Scion versions (and by git itself) still seeds:
// branch upstreams, push.autoSetupRemote, gc and maintenance keys, the origin
// remote. The strip predicate still removes the seed branch's stale upstream and
// push.autoSetupRemote.
func TestPrepareSeededGitConfig_EarlierBaseSeeds(t *testing.T) {
	raw := strings.Join([]string{
		"[core]",
		"\trepositoryformatversion = 0",
		"\tfilemode = true",
		"\tbare = false",
		"\tlogallrefupdates = true",
		`[remote "origin"]`,
		"\turl = https://github.com/o/r.git",
		"\tfetch = +refs/heads/*:refs/remotes/origin/*",
		`[branch "main"]`,
		"\tremote = origin",
		"\tmerge = refs/heads/main",
		`[branch "agent-x"]`,
		"\tremote = origin",
		"\tmerge = refs/heads/stale",
		"[push]",
		"\tautoSetupRemote = true",
		"[gc]",
		"\tauto = 0",
		"[maintenance]",
		"\tauto = false",
		"",
	}, "\n")
	out, stripped, err := PrepareSeededGitConfig([]byte(raw), RepoPolicyLinked, seedTestStrip)
	if err != nil {
		t.Fatalf("earlier base refused: %v", err)
	}
	if len(stripped) != 0 {
		t.Errorf("nothing credential-bearing, but stripped = %v", stripped)
	}
	vars, err := parseGitConfigFile(out)
	if err != nil {
		t.Fatalf("output does not parse: %v\n%s", err, out)
	}
	got := map[string]string{}
	for _, v := range vars {
		got[v.Key()] = v.Value
	}
	for k, want := range map[string]string{
		"core.repositoryformatversion": "0",
		"remote.origin.url":            "https://github.com/o/r.git",
		"remote.origin.fetch":          "+refs/heads/*:refs/remotes/origin/*",
		"branch.main.remote":           "origin",
		"branch.main.merge":            "refs/heads/main",
		"gc.auto":                      "0",
		"maintenance.auto":             "false",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q\n%s", k, got[k], want, out)
		}
	}
	for _, k := range []string{"branch.agent-x.remote", "branch.agent-x.merge", "push.autosetupremote"} {
		if _, ok := got[k]; ok {
			t.Errorf("strip predicate did not remove %s:\n%s", k, out)
		}
	}
}

// A key the redactor fully drops (credential.*, http.extraHeader, ...) is
// exempt from the allow-list: the seed continues and the key is absent from the
// copy.
func TestPrepareSeededGitConfig_DroppedKeysExemptAndRemoved(t *testing.T) {
	raw := strings.Join([]string{
		`[remote "origin"]`,
		"\turl = https://github.com/o/r.git",
		"[credential]",
		"\thelper = store --file=/tmp/credmarker-a1",
		`[credential "https://github.com"]`,
		"\tusername = credmarker-a2",
		"[http]",
		"\textraHeader = Authorization: credmarker-a3",
		"\tsslKey = /tmp/credmarker-a4",
		"[sendemail]",
		"\tsmtpPass = credmarker-a5",
		"",
	}, "\n")
	out, stripped, err := PrepareSeededGitConfig([]byte(raw), RepoPolicyLinked, seedTestStrip)
	if err != nil {
		t.Fatalf("a base with dropped keys must seed, got: %v", err)
	}
	if strings.Contains(string(out), "credmarker") || strings.Contains(string(out), "credential") {
		t.Errorf("dropped key survived in the copy:\n%s", out)
	}
	if len(stripped) == 0 {
		t.Error("dropped keys were not noted as stripped")
	}
	if !strings.Contains(string(out), "https://github.com/o/r.git") {
		t.Errorf("origin url lost:\n%s", out)
	}
}

// Include directives refuse at seed, and the include check runs before the
// exemption: even an exemption that matched everything cannot let one through.
func TestPrepareSeededGitConfig_IncludesRefuseBeforeExemption(t *testing.T) {
	for _, tc := range []struct{ raw, key string }{
		{"[include]\n\tpath = /tmp/incmarker-b1\n", "include.path"},
		{"[includeIf \"gitdir:/w/\"]\n\tpath = /tmp/incmarker-b1\n", "includeif.<redacted>.path"},
		{"[includeIf \"hasconfig:remote.*.url:https://h.example/**\"]\n\tpath = /tmp/incmarker-b1\n", "includeif.hasconfig:remote.*.url:https://h.example/<redacted>.path"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			r := mustRefuseSeed(t, tc.raw, tc.key, "incmarker-b1")
			if r.Reason != "an include directive" {
				t.Errorf("reason = %q, want the include reason", r.Reason)
			}
			vars, err := parseGitConfigFile([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			all := func(gitConfigVar) bool { return true }
			err = checkRepoConfigVars("", "", vars, RepoPolicyLinked, repoConfigAllowOptions{seed: true}, all)
			if err == nil {
				t.Fatal("an exemption matching everything let an include directive through")
			}
		})
	}
}

// http.* outside the drop set, url.*.insteadOf and the remote command and proxy
// overrides are not on RepoPolicyLinked and are not exempt, so they refuse at seed.
func TestPrepareSeededGitConfig_RedirectAndProxyKeysRefuse(t *testing.T) {
	for _, tc := range []struct{ raw, key string }{
		{"[http]\n\tproxy = http://redirmarker-c1:3128\n", "http.proxy"},
		{"[http \"https://h.example/\"]\n\tproxy = http://redirmarker-c1:3128\n", "http.https://h.example/.proxy"},
		{"[http]\n\tsslCAInfo = /tmp/redirmarker-c1.pem\n", "http.sslcainfo"},
		{"[http]\n\tsslCAPath = /tmp/redirmarker-c1\n", "http.sslcapath"},
		{"[url \"https://h2.example/\"]\n\tinsteadOf = https://redirmarker-c1.example/\n", "url.https://h2.example/.insteadof"},
		{"[url \"https://h.example/\"]\n\tpushInsteadOf = https://redirmarker-c1.example/\n", "url.https://h.example/.pushinsteadof"},
		{"[remote \"origin\"]\n\tproxy = http://redirmarker-c1:3128\n", "remote.origin.proxy"},
		{"[remote \"origin\"]\n\tuploadpack = redirmarker-c1\n", "remote.origin.uploadpack"},
		{"[remote \"origin\"]\n\treceivepack = redirmarker-c1\n", "remote.origin.receivepack"},
		{"[remote \"origin\"]\n\tvcs = redirmarker-c1\n", "remote.origin.vcs"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			_ = mustRefuseSeed(t, tc.raw, tc.key, "redirmarker-c1")
		})
	}
}

// Every key in seedUnpinnedRefuse refuses at seed, cannot be exempted, and is
// still allowed host-side (where HostGitArgs pins it).
func TestPrepareSeededGitConfig_UnpinnedKeysRefuse(t *testing.T) {
	if len(seedUnpinnedRefuse) == 0 {
		t.Fatal("seedUnpinnedRefuse is empty")
	}
	for _, r := range seedUnpinnedRefuse {
		if r.HasSubsection {
			t.Fatalf("row %+v has a subsection; extend this test", r)
		}
		key := r.Section + "." + r.Name
		t.Run(key, func(t *testing.T) {
			raw := "[" + r.Section + "]\n\t" + r.Name + " = /tmp/pinmarker-d1\n"
			_ = mustRefuseSeed(t, raw, key, "pinmarker-d1")

			v := seedConfigVar(key, "/tmp/pinmarker-d1")
			all := func(gitConfigVar) bool { return true }
			if err := checkRepoConfigVars("", "", []gitConfigVar{v}, RepoPolicyLinked, repoConfigAllowOptions{seed: true}, all); err == nil {
				t.Error("an exemption matching everything let an unpinned command key through")
			}
			if err := checkRepoConfigVars("/r", "", []gitConfigVar{v}, RepoPolicyLinked, repoConfigAllowOptions{}, nil); err != nil {
				t.Errorf("host-side check changed: %v", err)
			}
		})
	}
}

// A subsectioned form of a seedUnpinnedRefuse key is not a table hit (rows
// match exactly), but it still refuses at seed: allowVar allows core keys only
// at top level.
func TestPrepareSeededGitConfig_SubsectionedUnpinnedKeysRefuse(t *testing.T) {
	for _, r := range seedUnpinnedRefuse {
		key := r.Section + ".x." + r.Name
		t.Run(key, func(t *testing.T) {
			v := seedConfigVar(key, "/tmp/subpinmarker-j1")
			if seedKeyMatchesAny(seedUnpinnedRefuse, v) {
				t.Fatalf("%s matched a seedUnpinnedRefuse row; this test expects the allowVar path", key)
			}
			raw := "[" + r.Section + " \"x\"]\n\t" + r.Name + " = /tmp/subpinmarker-j1\n"
			refused := mustRefuseSeed(t, raw, key, "subpinmarker-j1")
			if refused.Reason == "a setting that runs a command in the agent's git" {
				t.Errorf("%s refused by the seed table, want allowVar: %q", key, refused.Reason)
			}
		})
	}
}

// Every key in seedUnpinnedSafe seeds unchanged.
func TestPrepareSeededGitConfig_UnpinnedSafeKeysSeed(t *testing.T) {
	if len(seedUnpinnedSafe) == 0 {
		t.Fatal("seedUnpinnedSafe is empty")
	}
	for _, r := range seedUnpinnedSafe {
		if r.HasSubsection {
			t.Fatalf("row %+v has a subsection; extend this test", r)
		}
		key := r.Section + "." + r.Name
		t.Run(key, func(t *testing.T) {
			raw := "[" + r.Section + "]\n\t" + r.Name + " = true\n"
			out, _, err := PrepareSeededGitConfig([]byte(raw), RepoPolicyLinked, seedTestStrip)
			if err != nil {
				t.Fatalf("seed refused %s: %v", key, err)
			}
			vars, err := parseGitConfigFile(out)
			if err != nil || len(vars) != 1 || vars[0].Key() != key || vars[0].Value != "true" {
				t.Errorf("%s not seeded unchanged (%v):\n%s", key, err, out)
			}
		})
	}
}

// Guard: every -c pin key in HostGitArgs that RepoPolicyLinked allows is in
// exactly one of seedUnpinnedRefuse and seedUnpinnedSafe, and every row of those
// tables is such a key. A pin added to HostGitArgs without a row fails here.
func TestSeedUnpinnedTables_CoverHostGitArgsPins(t *testing.T) {
	// Every -c pin a host-side command can carry: the fixed pins and the
	// network transport pins.
	args := append(HostGitArgs(), hostGitNetworkArgs...)
	allowedPins := map[seedKeyRule]bool{}
	refusedByLinked := map[string]bool{}
	for i := 0; i < len(args); i++ {
		if args[i] != "-c" {
			continue
		}
		if i+1 >= len(args) {
			t.Fatalf("HostGitArgs ends with a bare -c: %q", args)
		}
		i++
		key, value, ok := strings.Cut(args[i], "=")
		if !ok {
			t.Fatalf("HostGitArgs pin %q has no value", args[i])
		}
		// A pin may have a subsection (protocol.ext.allow); it is checked as
		// exactly the key a repository value would set.
		v := seedConfigVar(key, value)
		if ok, _ := allowVar(RepoPolicyLinked, v, repoConfigAllowOptions{}); !ok {
			refusedByLinked[v.Key()] = true
			continue // Linked refuses the key itself; nothing reaches the seed.
		}
		allowedPins[seedKeyRule{Section: v.Section, Subsection: v.Subsection, HasSubsection: v.HasSubsection, Name: v.Name}] = true
		inRefuse := seedKeyMatchesAny(seedUnpinnedRefuse, v)
		inSafe := seedKeyMatchesAny(seedUnpinnedSafe, v)
		if inRefuse == inSafe {
			t.Errorf("pin key %s is allowed by Linked and must be in exactly one of seedUnpinnedRefuse and seedUnpinnedSafe (refuse=%v safe=%v)", v.Key(), inRefuse, inSafe)
		}
	}
	if len(allowedPins) == 0 {
		t.Fatal("no HostGitArgs pin key is allowed by Linked; the guard checks nothing")
	}
	// The subsectioned pins are visited, not skipped, and Linked refuses them.
	for _, k := range []string{"protocol.ext.allow", "protocol.file.allow", "protocol.allow", "protocol.https.allow"} {
		if !refusedByLinked[k] {
			t.Errorf("pin %s was not visited as a key Linked refuses", k)
		}
	}
	// Every row is an allowed pin, compared by section, subsection and name.
	for name, table := range map[string][]seedKeyRule{"seedUnpinnedRefuse": seedUnpinnedRefuse, "seedUnpinnedSafe": seedUnpinnedSafe} {
		for _, r := range table {
			if !allowedPins[r] {
				t.Errorf("%s row %+v is not a HostGitArgs pin key that Linked allows", name, r)
			}
		}
	}
}

// seedKeyRule matches exactly: the subsection must agree, and a rule without
// one does not match a key with one.
func TestSeedKeyRule_MatchesExactly(t *testing.T) {
	top := seedKeyRule{Section: "core", Name: "pager"}
	sub := seedKeyRule{Section: "protocol", Subsection: "ext", HasSubsection: true, Name: "allow"}
	for _, tc := range []struct {
		r    seedKeyRule
		key  string
		want bool
	}{
		{top, "core.pager", true},
		{top, "core.x.pager", false},
		{top, "core.editor", false},
		{sub, "protocol.ext.allow", true},
		{sub, "protocol.file.allow", false},
		{sub, "protocol.allow", false},
		{sub, "protocol.EXT.allow", false},
	} {
		if got := tc.r.matches(seedConfigVar(tc.key, "x")); got != tc.want {
			t.Errorf("%+v.matches(%s) = %v, want %v", tc.r, tc.key, got, tc.want)
		}
	}
}

// Guard (exemption scope): no gitConfigDropRules entry matches an include
// directive, a seedUnpinnedRefuse key, or a key allowVar refuses for a specific
// reason (a command, program, proxy, redirect or similar). Keys allowVar refuses
// only by deny-by-default (the generic reason), such as core.sshCommand, are
// not caught here; TestGitConfigDropRules_NoKnownExecOrRedirectKey checks those
// against an explicit list.
func TestGitConfigDropRules_ExemptNoCommandRedirectOrInclude(t *testing.T) {
	const generic = "not on the linked-checkout allow-list"
	// Real names under a wildcard rule, plus one unknown name. (A wildcard such as
	// credential.* also matches an invented credential.program; such a key is
	// exempt only to be dropped, which TestPrepareSeededGitConfig_ExemptVarsAreDropped
	// pins.)
	probeNames := []string{"helper", "username", "usehttppath", "x"}
	for _, r := range gitConfigDropRules {
		if r.Section == "include" || r.Section == "includeif" {
			t.Errorf("drop rule %+v matches an include directive", r)
			continue
		}
		if r.Name == "program" {
			t.Errorf("drop rule %+v matches a program key", r)
		}
		names := []string{r.Name}
		if r.Name == "*" {
			names = probeNames
		}
		for _, name := range names {
			for _, sub := range []string{"", "https://h.example/"} {
				for _, value := range []string{"", "x", "!sh -c x", "ext::sh -c x"} {
					v := gitConfigVar{Section: r.Section, Subsection: sub, HasSubsection: sub != "", Name: name, Value: value}
					if !r.matches(v) {
						t.Fatalf("probe %+v does not match its own rule %+v", v, r)
					}
					if seedKeyMatchesAny(seedUnpinnedRefuse, v) {
						t.Errorf("drop rule %+v matches seedUnpinnedRefuse key %s", r, v.Key())
					}
					if ok, reason := allowVar(RepoPolicyLinked, v, repoConfigAllowOptions{}); !ok && reason != generic {
						t.Errorf("drop rule %+v matches %s, which Linked refuses as %q", r, v.Key(), reason)
					}
				}
			}
		}
	}
}

// Invariant: the bytes returned are serializeGitConfig of the
// variables that were checked, after redaction and the strip predicate; raw is
// parsed exactly once; an oddly quoted subsection is checked and written as the
// same key.
func TestPrepareSeededGitConfig_WritesTheCheckedParse(t *testing.T) {
	raw := strings.Join([]string{
		`[remote "or\"ig in"]`,
		"\turl = https://u:invmarker-e1@h.example/o/r.git",
		"\tfetch = +refs/heads/*:refs/remotes/x/*",
		`[branch "fe\\at\"ure] [core"]`,
		"\tremote = origin",
		"\tmerge = refs/heads/feature",
		"[Branch.MAIN]",
		"\tremote = origin",
		`[branch "agent-x"]`,
		"\tremote = origin",
		"[credential]",
		"\thelper = store",
		"",
	}, "\n")
	parses := 0
	var checked []gitConfigVar
	countingParse := func(b []byte) ([]gitConfigVar, error) {
		parses++
		vars, err := parseGitConfigFile(b)
		checked = append([]gitConfigVar(nil), vars...)
		return vars, err
	}
	out, _, err := prepareSeededGitConfig([]byte(raw), countingParse, RepoPolicyLinked, seedTestStrip)
	if err != nil {
		t.Fatalf("prepareSeededGitConfig: %v", err)
	}
	if parses != 1 {
		t.Fatalf("raw parsed %d times, want exactly 1", parses)
	}
	// Recompute from the variables that parse returned, which are the ones checked.
	if err := checkRepoConfigVars("", "", checked, RepoPolicyLinked, repoConfigAllowOptions{seed: true}, isDroppedKey); err != nil {
		t.Fatalf("the checked vars do not pass the check: %v", err)
	}
	kept, _ := redactGitConfigVars(checked)
	var want []gitConfigVar
	for _, v := range kept {
		if !seedTestStrip(v.Section, v.Subsection, v.HasSubsection, v.Name) {
			want = append(want, v)
		}
	}
	if string(out) != string(serializeGitConfig(want)) {
		t.Fatalf("output is not the serialization of the checked, filtered vars:\ngot:\n%s\nwant:\n%s", out, serializeGitConfig(want))
	}
	if strings.Contains(string(out), "invmarker-e1") {
		t.Errorf("userinfo survived:\n%s", out)
	}
	// Re-reading the output yields exactly the checked keys that survived.
	reread, err := parseGitConfigFile(out)
	if err != nil {
		t.Fatalf("output does not re-parse: %v", err)
	}
	keys := func(vs []gitConfigVar) []string {
		var ks []string
		for _, v := range vs {
			ks = append(ks, v.Key())
		}
		return ks
	}
	wantKeys := []string{
		`remote.or"ig in.url`,
		`remote.or"ig in.fetch`,
		`branch.fe\at"ure] [core.remote`,
		`branch.fe\at"ure] [core.merge`,
		"branch.main.remote",
	}
	if got := keys(reread); !reflect.DeepEqual(got, wantKeys) {
		t.Errorf("re-read keys = %q, want %q", got, wantKeys)
	}

	// The same quoting cannot smuggle a refused key past the check: the key that
	// is checked is the key git would read.
	smuggle := "[remote \"a\\\"] [core\"]\n\tuploadpack = invmarker-e2\n"
	_ = mustRefuseSeed(t, smuggle, "remote.<redacted>.uploadpack", "invmarker-e2")
}

// The functions on the seed path after the single parse work only on parsed
// variables: none of them reads a file, runs a command, or calls a parser or the
// raw-bytes helpers. With the parse-count test above, this shows the raw bytes
// are parsed exactly once.
func TestPrepareSeededGitConfig_NoReparseByStructure(t *testing.T) {
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	for _, file := range []string{"git_config_policy.go", "git_private_config.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	banned := map[string]bool{
		"parseGitConfigFile": true, "parseRepoConfig": true, "repoConfigFiles": true,
		"RedactGitConfigSecrets": true, "StripGitConfigKeys": true, "checkRepoConfigPolicy": true,
		"crossCheckAgainstGit": true,
	}
	bannedPkgs := map[string]bool{"os": true, "exec": true}
	for _, name := range []string{"prepareSeededGitConfig", "checkRepoConfigVars", "redactGitConfigVars", "allowVar"} {
		fd := funcs[name]
		if fd == nil {
			t.Fatalf("function %s not found", name)
		}
		parseCalls := 0
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if banned[x.Name] {
					t.Errorf("%s references %s", name, x.Name)
				}
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && bannedPkgs[id.Name] {
					t.Errorf("%s uses %s.%s", name, id.Name, x.Sel.Name)
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "parse" {
					parseCalls++
				}
			}
			return true
		})
		want := 0
		if name == "prepareSeededGitConfig" {
			want = 1
		}
		if parseCalls != want {
			t.Errorf("%s calls parse %d times, want %d", name, parseCalls, want)
		}
	}
	// raw is used exactly once in prepareSeededGitConfig: by the single parse.
	rawRefs := 0
	ast.Inspect(funcs["prepareSeededGitConfig"].Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "raw" {
			rawRefs++
		}
		return true
	})
	if rawRefs != 1 {
		t.Errorf("prepareSeededGitConfig references raw %d times, want 1 (the parse)", rawRefs)
	}
	// The exported entry point only hands the real parser to the inner function.
	var calls []string
	ast.Inspect(funcs["PrepareSeededGitConfig"].Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok {
				calls = append(calls, id.Name)
			}
		}
		return true
	})
	if !reflect.DeepEqual(calls, []string{"prepareSeededGitConfig"}) {
		t.Errorf("PrepareSeededGitConfig calls %v, want only prepareSeededGitConfig", calls)
	}
}

// The seedUnpinnedSafe conditions, proven: the repository cannot choose the
// command the safe keys would lead git to run, and pin keys Linked refuses
// outright refuse at seed. core.sshCommand is refused by allowVar itself, not
// through seedUnpinnedRefuse.
func TestPrepareSeededGitConfig_SafeRowConditions(t *testing.T) {
	// Refused at seed, each by the policy itself.
	for _, tc := range []struct{ raw, key string }{
		// submodule.recurse: no update command and no non-https/ssh URL.
		{"[submodule \"s\"]\n\tupdate = !sh -c condmarker-f1\n", "submodule.s.update"},
		{"[submodule \"s\"]\n\turl = ext::sh -c condmarker-f1\n", "submodule.s.url"},
		{"[submodule \"s\"]\n\turl = file:///tmp/condmarker-f1\n", "submodule.s.url"},
		// commit.gpgsign and tag.gpgsign: no signing-program key of any format.
		{"[gpg]\n\tprogram = /tmp/condmarker-f1\n", "gpg.program"},
		{"[gpg \"ssh\"]\n\tprogram = /tmp/condmarker-f1\n", "gpg.ssh.program"},
		{"[gpg \"openpgp\"]\n\tprogram = /tmp/condmarker-f1\n", "gpg.openpgp.program"},
		{"[gpg \"x509\"]\n\tprogram = /tmp/condmarker-f1\n", "gpg.x509.program"},
		{"[gpg \"ssh\"]\n\tdefaultKeyCommand = /tmp/condmarker-f1\n", "gpg.ssh.defaultkeycommand"},
		// HostGitArgs pin keys that Linked refuses outright never reach the seed.
		{"[merge]\n\tverifySignatures = condmarker-f1\n", "merge.verifysignatures"},
		{"[fetch]\n\trecurseSubmodules = condmarker-f1\n", "fetch.recursesubmodules"},
		{"[protocol \"ext\"]\n\tallow = condmarker-f1\n", "protocol.ext.allow"},
		{"[protocol \"file\"]\n\tallow = condmarker-f1\n", "protocol.file.allow"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			_ = mustRefuseSeed(t, tc.raw, tc.key, "condmarker-f1")
		})
	}

	// core.sshCommand: refused by allowVar(Linked) directly, host-side too, and
	// not merely by the seed-only table.
	ssh := seedConfigVar("core.sshCommand", "ssh")
	if seedKeyMatchesAny(seedUnpinnedRefuse, ssh) || seedKeyMatchesAny(seedUnpinnedSafe, ssh) {
		t.Error("core.sshcommand is in a seed-unpinned table; it must be refused by the policy itself")
	}
	if ok, _ := allowVar(RepoPolicyLinked, ssh, repoConfigAllowOptions{}); ok {
		t.Error("allowVar(Linked) allows core.sshcommand")
	}

	// With the conditions holding, submodule.recurse and signing seed together
	// with value-checked submodule and signing-format settings.
	raw := strings.Join([]string{
		"[submodule]",
		"\trecurse = true",
		`[submodule "s"]`,
		"\turl = https://github.com/o/s.git",
		"\tupdate = checkout",
		"[commit]",
		"\tgpgsign = true",
		"[tag]",
		"\tgpgsign = true",
		"[gpg]",
		"\tformat = ssh",
		"[user]",
		"\tsigningKey = ~/.ssh/id_ed25519.pub",
		"",
	}, "\n")
	if _, _, err := PrepareSeededGitConfig([]byte(raw), RepoPolicyLinked, seedTestStrip); err != nil {
		t.Errorf("value-checked submodule and signing settings refused: %v", err)
	}
}

// Every variable the seed exempts from the allow-list (isDroppedKey) is removed
// by the redaction on the same parse, so an exempt key never reaches the copy.
func TestPrepareSeededGitConfig_ExemptVarsAreDropped(t *testing.T) {
	var vars []gitConfigVar
	for _, r := range gitConfigDropRules {
		name := r.Name
		if name == "*" {
			name = "helper"
		}
		for _, sub := range []string{"", "https://h.example/"} {
			vars = append(vars, gitConfigVar{Section: r.Section, Subsection: sub, HasSubsection: sub != "", Name: name, Value: "!sh -c dropmarker-g1"})
		}
	}
	for _, v := range vars {
		if !isDroppedKey(v) {
			t.Fatalf("probe %s is not exempt", v.Key())
		}
	}
	if err := checkRepoConfigVars("", "", vars, RepoPolicyLinked, repoConfigAllowOptions{seed: true}, isDroppedKey); err != nil {
		t.Fatalf("exempt vars refused: %v", err)
	}
	kept, _ := redactGitConfigVars(vars)
	if len(kept) != 0 {
		t.Errorf("exempt vars survived redaction: %+v", kept)
	}
	out, _, err := PrepareSeededGitConfig(serializeGitConfig(vars), RepoPolicyLinked, nil)
	if err != nil || len(out) != 0 {
		t.Errorf("seed of only exempt keys = %q, %v; want empty output and no error", out, err)
	}
}

// Guard (exemption scope, explicit list): no gitConfigDropRules entry matches a
// known command, redirect, program or include key, whatever reason allowVar
// gives for it. credential.helper is a command-capable key the drop rules match
// on purpose: it is exempt only to be dropped
// (TestPrepareSeededGitConfig_ExemptVarsAreDropped), so it is not listed.
func TestGitConfigDropRules_NoKnownExecOrRedirectKey(t *testing.T) {
	for _, key := range []string{
		"core.sshcommand", "core.editor", "core.askpass", "core.gitproxy",
		"core.hookspath", "core.fsmonitor", "core.pager", "core.worktree",
		"sequence.editor", "diff.external", "pager.log", "interactive.difffilter",
		"alias.x", "url.x.insteadof", "url.x.pushinsteadof",
		"http.proxy", "http.x.proxy", "remote.x.proxy",
		"remote.x.uploadpack", "remote.x.receivepack", "remote.x.vcs",
		"gpg.program", "gpg.ssh.program", "gpg.openpgp.program", "gpg.x509.program",
		"gpg.ssh.defaultkeycommand", "x.program",
		"imap.tunnel", "sendemail.sendmailcmd", "sendemail.smtpserver",
		"lfs.customtransfer.x.path", "lfs.standalonetransferagent",
		"filter.x.clean", "filter.x.smudge", "filter.x.process",
		"diff.x.textconv", "diff.x.command", "merge.x.driver",
		"submodule.x.update", "protocol.allow", "protocol.x.allow",
		"uploadpack.packobjectshook", "include.path", "includeif.x.path",
		"difftool.x.cmd", "mergetool.x.cmd", "browser.x.cmd",
		"sendemail.tocmd", "sendemail.cccmd", "core.alternaterefscommand",
	} {
		v := seedConfigVar(key, "x")
		if isDroppedKey(v) {
			t.Errorf("drop rule matches %s: the drop exemption would skip the allow-list for it", key)
		}
	}
}

// A seed refusal never prints userinfo carried in a URL subsection: the key is
// named by notedKeyName, which still names the section and variable.
func TestPrepareSeededGitConfig_RefusalRedactsSubsectionUserinfo(t *testing.T) {
	for _, tc := range []struct{ raw, key string }{
		{"[url \"https://u:keymarker-k1@h.example/\"]\n\tinsteadOf = https://github.com/\n", "url.https://h.example/<redacted>.insteadof"},
		{"[http \"https://u:keymarker-k1@h.example/\"]\n\tsslVerify = false\n", "http.https://h.example/<redacted>.sslverify"},
		{"[includeIf \"hasconfig:remote.*.url:https://u:keymarker-k1@h.example/**\"]\n\tpath = /tmp/inc\n", "includeif.hasconfig:remote.*.url:https://h.example/<redacted>.path"},
		{"[lfs \"https://u:keymarker-k1@h.example/\"]\n\tlocksverify = notabool\n", "lfs.https://h.example/<redacted>.locksverify"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			r := mustRefuseSeed(t, tc.raw, tc.key, "keymarker-k1")
			if !strings.HasPrefix(r.Key, strings.SplitN(tc.key, ".", 2)[0]+".") {
				t.Errorf("redacted key %q lost its section", r.Key)
			}
		})
	}
}

// Host-side refusals (checkRepoConfigPolicy, the HostGitCommand path) name
// the key by notedKeyName too: a userinfo marker in a url, http or lfs
// subsection is absent from Error() and Key. A plain key keeps the host
// wording byte for byte.
func TestCheckRepoConfigPolicy_RefusalRedactsSubsectionUserinfo(t *testing.T) {
	hostRefusal := func(t *testing.T, body string) (string, *RepoConfigRefusedError) {
		t.Helper()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		// The refusal comes from the as-data check, before git is run.
		err := checkRepoConfigPolicy(context.Background(), dir, nil, RepoPolicyLinked, repoConfigAllowOptions{})
		var refused *RepoConfigRefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("error = %v, want *RepoConfigRefusedError", err)
		}
		if refused.Seeded {
			t.Error("host refusal is marked Seeded")
		}
		return dir, refused
	}
	for _, tc := range []struct{ body, key string }{
		{"[url \"https://u:hostmarker-m1@h.example/\"]\n\tinsteadOf = https://github.com/\n", "url.https://h.example/<redacted>.insteadof"},
		{"[http \"https://u:hostmarker-m1@h.example/\"]\n\tsslVerify = false\n", "http.https://h.example/<redacted>.sslverify"},
		{"[lfs \"https://u:hostmarker-m1@h.example/\"]\n\tlocksverify = notabool\n", "lfs.https://h.example/<redacted>.locksverify"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			_, r := hostRefusal(t, tc.body)
			if r.Key != tc.key {
				t.Errorf("refused key = %q, want %q", r.Key, tc.key)
			}
			if strings.Contains(r.Key, "hostmarker-m1") || strings.Contains(r.Error(), "hostmarker-m1") {
				t.Errorf("host refusal leaks userinfo: key %q, error %q", r.Key, r.Error())
			}
		})
	}
	t.Run("credential subsection always redacted", func(t *testing.T) {
		_, r := hostRefusal(t, "[credential \"https://github.com\"]\n\tusername = x\n")
		if r.Key != "credential.<redacted>.username" {
			t.Errorf("refused key = %q, want credential.<redacted>.username", r.Key)
		}
	})
	t.Run("plain key wording unchanged", func(t *testing.T) {
		dir, r := hostRefusal(t, "[core]\n\tsshCommand = ssh -v\n")
		want := "git in " + dir + " refused: repository config sets core.sshcommand, which is not on the linked-checkout allow-list; remove it from the repository's .git/config (or config.worktree)"
		if r.Error() != want {
			t.Errorf("host wording = %q, want %q", r.Error(), want)
		}
	})
}

// A base that does not parse fails closed with the parse error, not a refusal.
func TestPrepareSeededGitConfig_ParseErrorFailsClosed(t *testing.T) {
	out, stripped, err := PrepareSeededGitConfig([]byte("[core\n\tbare = false\n"), RepoPolicyLinked, seedTestStrip)
	if err == nil {
		t.Fatalf("malformed base accepted; output:\n%s", out)
	}
	if out != nil || stripped != nil {
		t.Errorf("parse error returned output %q, stripped %v", out, stripped)
	}
	var refused *RepoConfigRefusedError
	if errors.As(err, &refused) {
		t.Errorf("parse error reported as a policy refusal: %v", err)
	}
}

// Guard: newRepoConfigRefusedError is the only place a RepoConfigRefusedError
// is built, anywhere in the module's non-test code, so every refusal key is
// redacted and every refusal carries the audience it was built for. It flags,
// outside that constructor:
//   - a composite literal of the type, in a function or at package level;
//   - new(RepoConfigRefusedError);
//   - an assignment to the Key, Layer or Audience field of a variable declared
//     with the type (or bound from the constructor).
//
// Without type checking, the Key check sees only variables whose declaration
// names the type; the literal and new() checks see every construction.
func TestRepoConfigRefusedError_SingleConstructor(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s: %v", root, err)
	}
	const typeName = "RepoConfigRefusedError"
	isType := func(e ast.Expr) bool {
		if st, ok := e.(*ast.StarExpr); ok {
			e = st.X
		}
		switch x := e.(type) {
		case *ast.Ident:
			return x.Name == typeName
		case *ast.SelectorExpr:
			return x.Sel.Name == typeName
		}
		return false
	}
	fset := token.NewFileSet()
	checked := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			// A nested module is not part of this module.
			if path != root {
				if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(src), typeName) {
			return nil
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		checked++
		// The constructor body is the only allowed site.
		var allowed *ast.BlockStmt
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "newRepoConfigRefusedError" && f.Name.Name == "util" {
				allowed = fd.Body
			}
		}
		typed := map[string]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				return false
			}
			if allowed != nil && n == ast.Node(allowed) {
				return false
			}
			switch x := n.(type) {
			case *ast.CompositeLit:
				if x.Type != nil && isType(x.Type) {
					t.Errorf("%s: RepoConfigRefusedError literal outside newRepoConfigRefusedError", fset.Position(x.Pos()))
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 && isType(x.Args[0]) {
					t.Errorf("%s: new(RepoConfigRefusedError) outside newRepoConfigRefusedError", fset.Position(x.Pos()))
				}
			case *ast.ValueSpec:
				if x.Type != nil && isType(x.Type) {
					for _, nm := range x.Names {
						typed[nm.Name] = true
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range x.Lhs {
					if i < len(x.Rhs) {
						if call, ok := x.Rhs[i].(*ast.CallExpr); ok {
							if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "newRepoConfigRefusedError" {
								if lid, ok := lhs.(*ast.Ident); ok {
									typed[lid.Name] = true
								}
							}
						}
					}
					if sel, ok := lhs.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Key" || sel.Sel.Name == "Layer" || sel.Sel.Name == "Audience") {
						if id, ok := sel.X.(*ast.Ident); ok && typed[id.Name] {
							t.Errorf("%s: assignment to RepoConfigRefusedError.%s outside newRepoConfigRefusedError", fset.Position(sel.Pos()), sel.Sel.Name)
						}
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no non-test file mentioning RepoConfigRefusedError was checked")
	}
}

// The conservative pass covers subsection forms notedKeyName keeps: the
// marker is absent, and the host and the variable name survive.
func TestPrepareSeededGitConfig_RefusalRedactsOtherUserinfoForms(t *testing.T) {
	for _, tc := range []struct{ name, raw, key string }{
		{"schemeless", "[url \"user:formmarker-n1@host.example\"]\n\tinsteadOf = https://github.com/\n", "url.<redacted>.insteadof"},
		{"scp with password", "[url \"git:formmarker-n1@host.example:o/r.git\"]\n\tinsteadOf = https://github.com/\n", "url.<redacted>.insteadof"},
		{"slash in password", "[url \"https://u:p/formmarker-n1@host.example/\"]\n\tinsteadOf = https://github.com/\n", "url.<redacted>.insteadof"},
		{"upper-case hasconfig", "[includeIf \"HASCONFIG:remote.*.url:https://u:formmarker-n1@host.example/**\"]\n\tpath = /tmp/inc\n", "includeif.hasconfig:remote.*.url:https://host.example/<redacted>.path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// mustRefuseSeed asserts the exact redacted key and that the marker
			// is absent from Key and Error().
			r := mustRefuseSeed(t, tc.raw, tc.key, "formmarker-n1")
			if tc.name == "upper-case hasconfig" && !strings.Contains(r.Key, "host.example") {
				t.Errorf("redacted key %q lost the host of a parseable URL", r.Key)
			}
		})
	}
	// Keys without '@' in the subsection are unchanged.
	for _, key := range []string{"core.sshcommand", "url.https://h.example/.insteadof", "branch.main.remote", ".git/commondir"} {
		if got := refusalKeyName(key); got != key {
			t.Errorf("refusalKeyName(%q) = %q, want it unchanged", key, got)
		}
	}
}

// A cross-check refusal (a key git reports that the parser did not) names the
// key git printed through the same redaction.
func TestCrossCheckAgainstGit_RefusalRedactsSubsectionUserinfo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[core]\n\trepositoryformatversion = 0\n[url \"https://u:xmarker-p1@h.example/\"]\n\tinsteadOf = https://github.com/\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(dir, ".git", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(HostGitEnv(os.Environ()), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	// Parsed set without the url key: git reports it, so the cross-check refuses.
	parsed := []gitConfigVar{{Section: "core", Name: "repositoryformatversion", Value: "0"}}
	err := crossCheckAgainstGit(context.Background(), dir, env, parsed, "", false, AudienceLocal)
	var refused *RepoConfigRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("error = %v, want *RepoConfigRefusedError", err)
	}
	if strings.Contains(refused.Key, "xmarker-p1") || strings.Contains(refused.Error(), "xmarker-p1") {
		t.Errorf("cross-check refusal leaks userinfo: key %q, error %q", refused.Key, refused.Error())
	}
	if refused.Key != "url.https://h.example/<redacted>.insteadof" {
		t.Errorf("refused key = %q, want url.https://h.example/<redacted>.insteadof", refused.Key)
	}
}

// refusalSinks renders err through every fmt verb and both slog handlers, as
// a refusal reaches logs and messages.
func refusalSinks(t *testing.T, err error) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		out[verb] = fmt.Sprintf(verb, err)
		out["wrapped "+verb] = fmt.Sprintf(verb, fmt.Errorf("start: %w", err))
	}
	var refused *RepoConfigRefusedError
	if errors.As(err, &refused) {
		out["deref %#v"] = fmt.Sprintf("%#v", *refused)
		out["deref %+v"] = fmt.Sprintf("%+v", *refused)
	}
	for name, h := range map[string]func(*bytes.Buffer) slog.Handler{
		"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var buf bytes.Buffer
		logger := slog.New(h(&buf))
		logger.Error("refused", "error", err)
		if refused != nil {
			logger.Error("refused", slog.Any("refusal", refused), slog.Any("copy", *refused))
		}
		out["slog "+name] = buf.String()
	}
	return out
}

// D-2: every fmt verb and every slog handler prints Error(), never the
// fields, for a refusal of either audience and a seeded one.
func TestRepoConfigRefusedError_FormatAndLogValue(t *testing.T) {
	const marker = "dirmarker-k4w8"
	for _, tc := range []struct {
		name string
		err  *RepoConfigRefusedError
	}{
		{"remote", newRepoConfigRefusedError("/srv/"+marker+"/repo", "", "core.sshcommand", "valmarker-z1", "x", false, AudienceRemote)},
		{"seeded", newRepoConfigRefusedError("", "", "core.sshcommand", "valmarker-z1", "x", true, AudienceRemote)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.err.Error()
			if strings.Contains(want, marker) || strings.Contains(want, "valmarker-z1") {
				t.Fatalf("Error() prints the directory or the value: %q", want)
			}
			for sink, got := range refusalSinks(t, tc.err) {
				if strings.Contains(got, marker) || strings.Contains(got, "valmarker-z1") || strings.Contains(got, "Dir") {
					t.Errorf("%s prints a field: %q", sink, got)
				}
			}
			for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
				if got := fmt.Sprintf(verb, tc.err); got != want {
					t.Errorf("Sprintf(%s) = %q, want exactly Error() %q", verb, got, want)
				}
			}
			v := slog.AnyValue(tc.err)
			if v.Kind() != slog.KindLogValuer {
				t.Fatalf("slog kind = %v, want LogValuer", v.Kind())
			}
			if got := v.Resolve().String(); got != want {
				t.Errorf("LogValue = %q, want Error() %q", got, want)
			}
		})
	}
	var nilRefusal *RepoConfigRefusedError
	if got := fmt.Sprintf("%+v", nilRefusal); got != "<nil>" {
		t.Errorf("nil refusal formats as %q", got)
	}
}

// The audience reaches the refusal through the constructor: AudienceRemote
// names no directory; AudienceLocal keeps the host wording byte for byte.
func TestRepoConfigRefusedError_Audience(t *testing.T) {
	local := newRepoConfigRefusedError("/r", "", "core.sshCommand", "v", "x", false, AudienceLocal).Error()
	if want := "git in /r refused: repository config sets core.sshCommand, which is x; remove it from the repository's .git/config (or config.worktree)"; local != want {
		t.Errorf("local wording = %q, want %q", local, want)
	}
	remote := newRepoConfigRefusedError("/r", "", "core.sshCommand", "v", "x", false, AudienceRemote).Error()
	if want := "git in the project repository refused: repository config sets core.sshCommand, which is x; remove it from the repository's .git/config (or config.worktree)"; remote != want {
		t.Errorf("remote wording = %q, want %q", remote, want)
	}
}

// D-1: a seeded cross-check refusal carries no directory, though git ran in
// one; every sink of it is free of the path.
func TestCrossCheckAgainstGit_SeededRefusalHasNoDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := filepath.Join(t.TempDir(), "seedmarker-d1q9")
	for _, d := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(dir, ".git", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "[core]\n\trepositoryformatversion = 0\n[user]\n\tname = n\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(HostGitEnv(os.Environ()), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	// The parsed set lacks user.name, which git reports: the cross-check refuses.
	parsed := []gitConfigVar{{Section: "core", Name: "repositoryformatversion", Value: "0"}}
	for _, seeded := range []bool{true, false} {
		err := crossCheckAgainstGit(context.Background(), dir, env, parsed, "", seeded, AudienceLocal)
		var refused *RepoConfigRefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("seeded=%v: error = %v, want *RepoConfigRefusedError", seeded, err)
		}
		if !seeded {
			// The host form still names its directory for the local CLI.
			if refused.Dir != dir {
				t.Errorf("host cross-check refusal Dir = %q, want %q", refused.Dir, dir)
			}
			continue
		}
		if refused.Dir != "" {
			t.Errorf("seeded cross-check refusal carries Dir %q", refused.Dir)
		}
		if refused.Audience != AudienceRemote {
			t.Errorf("seeded cross-check refusal audience = %v, want AudienceRemote", refused.Audience)
		}
		for sink, got := range refusalSinks(t, err) {
			if strings.Contains(got, "seedmarker-d1q9") {
				t.Errorf("%s prints the directory: %q", sink, got)
			}
		}
	}
}
