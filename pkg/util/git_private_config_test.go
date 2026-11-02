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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestRedactGitConfigSecrets(t *testing.T) {
	in := strings.Join([]string{
		"[core]",
		"\trepositoryformatversion = 0",
		"\tfilemode = true",
		"[remote \"origin\"]",
		"\turl = https://alice:s3cret@host.example/o/r.git",
		"\tfetch = +refs/heads/*:refs/remotes/origin/*",
		"\tfetch = +refs/tags/*:refs/tags/*",
		"[credential]",
		"\thelper = store",
		"[credential \"https://host.example\"]",
		"\tusername = alice",
		"[http \"https://host.example\"]",
		"\textraheader = Authorization: Basic QUJD",
		"\tsslVerify = true",
		"[url \"https://bob:pw@mirror.example/\"]",
		"\tinsteadOf = https://host.example/",
		"",
	}, "\n")

	out, stripped, err := RedactGitConfigSecrets([]byte(in))
	if err != nil {
		t.Fatalf("RedactGitConfigSecrets: %v", err)
	}
	outStr := string(out)

	// No secret values survive.
	for _, bad := range []string{"s3cret", "store", "Basic QUJD", "bob:pw", "username = alice", "alice:"} {
		if strings.Contains(outStr, bad) {
			t.Errorf("redacted config still contains %q:\n%s", bad, outStr)
		}
	}
	// Retained, non-secret keys and both multi-valued fetch entries survive.
	for _, keep := range []string{
		"repositoryformatversion", "filemode = ", "sslverify = ", "insteadof = ",
		"refs/heads/*:refs/remotes/origin/*", "refs/tags/*:refs/tags/*",
	} {
		if !strings.Contains(strings.ToLower(outStr), strings.ToLower(keep)) {
			t.Errorf("redacted config dropped expected key %q:\n%s", keep, outStr)
		}
	}
	// The origin URL is kept but without userinfo.
	if !strings.Contains(outStr, "https://host.example/o/r.git") {
		t.Errorf("origin url not preserved (userinfo-stripped):\n%s", outStr)
	}
	// The url.<base> subsection has its userinfo scrubbed too.
	if !strings.Contains(outStr, "https://mirror.example/") || strings.Contains(outStr, "bob") {
		t.Errorf("url.<base> subsection userinfo not scrubbed:\n%s", outStr)
	}

	// Noted key names are redacted — a credential.<url> subsection is always
	// "<redacted>", and a url.<base> base with userinfo appears as scheme://host
	// followed by "/<redacted>".
	wantStripped := []string{
		"credential.helper",
		"credential.<redacted>.username",
		"http.https://host.example.extraheader",
		"remote.origin.url",
		"url.https://mirror.example/<redacted>.insteadof",
	}
	got := append([]string(nil), stripped...)
	sort.Strings(got)
	sort.Strings(wantStripped)
	if strings.Join(got, "\n") != strings.Join(wantStripped, "\n") {
		t.Errorf("stripped keys = %v, want %v", got, wantStripped)
	}
	// No userinfo placeholder may appear in the stripped names either.
	for _, bad := range []string{"s3cret", "bob:pw", "alice"} {
		if strings.Contains(strings.Join(stripped, "\n"), bad) {
			t.Errorf("stripped names contain userinfo %q: %v", bad, stripped)
		}
	}

	// The result re-parses (it is valid git config).
	if _, err := parseGitConfigFile(out); err != nil {
		t.Errorf("redacted config does not re-parse: %v\n%s", err, outStr)
	}
}

// Covers the additional URL-valued keys and subsections: lfs.url / lfs.pushurl,
// remote.*.lfsurl / lfspushurl, an http.<base> subsection, http.*.cookiefile,
// and a URL-valued key whose value does not parse but carries userinfo.
func TestRedactGitConfigSecrets_URLKeyCoverage(t *testing.T) {
	in := strings.Join([]string{
		"[lfs]",
		"\turl = https://alice:s3cret@lfs.example/lfs",
		"[lfs]",
		"\tpushurl = https://alice:s3cret@lfs.example/push",
		"[remote \"origin\"]",
		"\tlfsurl = https://bob:pw@lfs.example/o/r",
		"\tlfspushurl = https://bob:pw@lfs.example/o/push",
		"[http \"https://carol:tok@host.example\"]",
		"\tsslVerify = true",
		"[http \"https://host.example\"]",
		"\tcookiefile = /home/alice/.gitcookies",
		"[remote \"weird\"]",
		"\turl = ht!tp://dave:pw@broken.example/x",
		"",
	}, "\n")

	out, stripped, err := RedactGitConfigSecrets([]byte(in))
	if err != nil {
		t.Fatalf("RedactGitConfigSecrets: %v", err)
	}
	outStr := string(out)

	for _, bad := range []string{"s3cret", "bob:pw", "carol:tok", "dave:pw", ".gitcookies", "cookiefile"} {
		if strings.Contains(outStr, bad) {
			t.Errorf("redacted config still contains %q:\n%s", bad, outStr)
		}
	}
	// The scrubbed LFS and http subsection URLs survive without userinfo.
	for _, keep := range []string{"https://lfs.example/lfs", "https://lfs.example/o/r", "https://host.example"} {
		if !strings.Contains(outStr, keep) {
			t.Errorf("redacted config dropped expected scrubbed URL %q:\n%s", keep, outStr)
		}
	}
	// The unparseable-with-userinfo key is dropped entirely.
	if strings.Contains(outStr, "broken.example") {
		t.Errorf("an unparseable URL value with userinfo must be dropped:\n%s", outStr)
	}
	for _, want := range []string{
		"lfs.url", "lfs.pushurl", "remote.origin.lfsurl", "remote.origin.lfspushurl",
		"http.https://host.example.cookiefile", "remote.weird.url",
	} {
		found := false
		for _, s := range stripped {
			if s == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in stripped keys, got %v", want, stripped)
		}
	}
	// No userinfo placeholder may appear in the stripped names.
	for _, bad := range []string{"s3cret", "bob:pw", "carol:tok", "dave:pw"} {
		if strings.Contains(strings.Join(stripped, "\n"), bad) {
			t.Errorf("stripped names contain userinfo %q: %v", bad, stripped)
		}
	}
	if _, err := parseGitConfigFile(out); err != nil {
		t.Errorf("redacted config does not re-parse: %v\n%s", err, outStr)
	}
}

func TestRedactGitConfigSecrets_ParseErrorFailsClosed(t *testing.T) {
	if _, _, err := RedactGitConfigSecrets([]byte("[lfs\n\tbroken")); err == nil {
		t.Error("an unparseable config should return an error (fail closed), got nil")
	}
}

// TestRedactedPrivateConfig_IsUsableByGit is the functional check for the
// private-config mechanism: a config run through RedactGitConfigSecrets and put
// back as a repository's .git/config is still a fully working config — git can
// commit, set local config (push -u's upstream recording), and push/fetch — while
// the credential material is gone. This exercises the re-serialisation on a real
// git.
func TestRedactedPrivateConfig_IsUsableByGit(t *testing.T) {
	hostGitTestEnv(t)
	root := t.TempDir()

	// A bare "remote".
	remote := filepath.Join(root, "remote.git")
	hostRunGit(t, root, "init", "-q", "--bare", remote)

	// A working repo with an origin and a credential-bearing config.
	repo := filepath.Join(root, "repo")
	hostRunGit(t, root, "init", "-q", "-b", "main", repo)
	hostRunGit(t, repo, "remote", "add", "origin", remote)
	hostRunGit(t, repo, "config", "--local", "http.https://host.example.extraheader", "Authorization: Basic QUJD")
	hostRunGit(t, repo, "config", "--local", "credential.helper", "store")

	// Redact the repo's own .git/config and write it back (what the seed does).
	cfgPath := filepath.Join(repo, ".git", "config")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	redacted, stripped, err := RedactGitConfigSecrets(raw)
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	if len(stripped) == 0 {
		t.Fatal("expected credential keys to be stripped")
	}
	if err := os.WriteFile(cfgPath, redacted, 0600); err != nil {
		t.Fatal(err)
	}

	// git still works with the redacted config: commit, push -u (writes
	// branch.main.{remote,merge} to the now-private config), a second commit and
	// push, and fetch.
	writeFile(t, filepath.Join(repo, "f.txt"), "hello\n")
	hostRunGit(t, repo, "add", ".")
	hostRunGit(t, repo, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "first")
	hostRunGit(t, repo, "push", "-q", "-u", "origin", "main")
	writeFile(t, filepath.Join(repo, "f.txt"), "hello again\n")
	hostRunGit(t, repo, "add", ".")
	hostRunGit(t, repo, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "second")
	hostRunGit(t, repo, "push", "-q", "origin", "main")
	hostRunGit(t, repo, "fetch", "-q", "origin")

	// The credential material is gone; the origin url remains.
	cfgNow, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"extraheader", "credential", "Basic QUJD"} {
		if strings.Contains(string(cfgNow), bad) {
			t.Errorf("redacted config still contains %q:\n%s", bad, cfgNow)
		}
	}
	if out := hostRunGit(t, repo, "config", "--local", "branch.main.remote"); out != "origin" {
		t.Errorf("push -u did not record the upstream in the private config: branch.main.remote=%q", out)
	}
}

func TestSanitizeURLUserinfo(t *testing.T) {
	cases := map[string]struct {
		want    string
		changed bool
		drop    bool
	}{
		"https://u:p@h/x": {"https://h/x", true, false},
		"https://u@h/x":   {"https://h/x", true, false},
		"https://h/x":     {"https://h/x", false, false},
		"git@h:o/r.git":   {"git@h:o/r.git", false, false}, // scp-style, kept unchanged
		"ssh://u@h/x":     {"ssh://u@h/x", false, false},   // ssh with no password, kept
		// Unparseable but URL-shaped ("://") and carries userinfo (an invalid percent
		// escape in the userinfo): drop the key so the userinfo cannot survive.
		"https://u:p%zz@host/x": {"", false, true},
		// Unparseable, URL-shaped, but no userinfo: kept unchanged.
		"https://host/x%zz": {"https://host/x%zz", false, false},
		// Unparseable scp-style base with userinfo but no "://": kept unchanged.
		"git@github.com:o/r.git": {"git@github.com:o/r.git", false, false},
		// remote-helper prefix: split, sanitize the remainder, re-prepend.
		"http::https://u:p@h/x": {"http::https://h/x", true, false},
		// helper prefix over an unparseable URL-shaped remainder with userinfo: drop.
		"http::https://u:p%zz@h/x": {"", false, true},
		// http family (http/https/ftp/ftps): any userinfo removed, username-only too.
		"ftp://u:p@h/x": {"ftp://h/x", true, false},
		"ftps://u@h/x":  {"ftps://h/x", true, false},
		// Any other scheme: a password strips ALL userinfo; a username with no
		// password is kept unchanged.
		"git://u:p@h/x":       {"git://h/x", true, false},       // password -> stripped
		"git://u@h/x":         {"git://u@h/x", false, false},    // username only -> kept
		"file://u:p@h/x":      {"file://h/x", true, false},      // password -> stripped
		"file://u@h/x":        {"file://u@h/x", false, false},   // username only -> kept
		"ssh://u:p@h/x":       {"ssh://h/x", true, false},       // password -> stripped
		"socks5://u:p@h:1080": {"socks5://h:1080", true, false}, // password -> stripped
		// Schemeless user:password@host is NOT handled by the shared helper (it is
		// scp form for non-proxy keys); the proxy-only helper handles proxy keys.
		"u:p@proxy.example:8080": {"u:p@proxy.example:8080", false, false},
		// scp host:path (opaque "@" after the colon) is kept by the shared helper.
		"host:repo@x.git": {"host:repo@x.git", false, false},
		// Bare host:port (scheme-like, no "@"): kept unchanged.
		"proxy.example:8080": {"proxy.example:8080", false, false},
	}
	for in, want := range cases {
		got, changed, drop := sanitizeURLUserinfo(in)
		if got != want.want || changed != want.changed || drop != want.drop {
			t.Errorf("sanitizeURLUserinfo(%q) = (%q,%v,%v), want (%q,%v,%v)", in, got, changed, drop, want.want, want.changed, want.drop)
		}
		// A changed/dropped result must not retain the synthetic password placeholder.
		if (changed || drop) && strings.Contains(got, ":p") {
			t.Errorf("sanitizeURLUserinfo(%q) result still contains a password: %q", in, got)
		}
	}
}

// TestRedactGitConfigSecrets_ScpStyleKept confirms scp-style git remotes and url
// bases (git@host:path, no "://") survive redaction byte-equal on both the value
// path and the subsection path, and nothing is reported stripped for them.
func TestRedactGitConfigSecrets_ScpStyleKept(t *testing.T) {
	in := strings.Join([]string{
		"[remote \"origin\"]",
		"\turl = git@github.com:o/r.git",
		"[url \"git@github.com:\"]",
		"\tinsteadOf = https://github.com/",
		"",
	}, "\n")

	out, stripped, err := RedactGitConfigSecrets([]byte(in))
	if err != nil {
		t.Fatalf("RedactGitConfigSecrets: %v", err)
	}
	outStr := string(out)

	if len(stripped) != 0 {
		t.Errorf("expected no keys stripped for scp-style config, got %v", stripped)
	}
	for _, keep := range []string{
		"[remote \"origin\"]\n\turl = \"git@github.com:o/r.git\"\n",
		"[url \"git@github.com:\"]\n\tinsteadof = \"https://github.com/\"\n",
	} {
		if !strings.Contains(outStr, keep) {
			t.Errorf("scp-style entry not kept byte-equal (%q):\n%s", keep, outStr)
		}
	}
	if _, err := parseGitConfigFile(out); err != nil {
		t.Errorf("redacted config does not re-parse: %v\n%s", err, outStr)
	}
}

// TestRedactGitConfigSecrets_SubsectionDropsOnUnparseableBase covers the
// subsection path: a url.<base>/http.<base> entry whose base cannot be parsed
// but carries userinfo is dropped entirely, so the userinfo never reaches the
// serialized section header. A sibling key with a clean base is kept.
func TestRedactGitConfigSecrets_SubsectionDropsOnUnparseableBase(t *testing.T) {
	in := strings.Join([]string{
		"[url \"https://u:p%zz@host.example\"]",
		"\tinsteadOf = https://host.example/",
		"[http \"https://u:p%zz@host.example\"]",
		"\tsslVerify = true",
		"[http \"https://clean.example\"]",
		"\tsslVerify = false",
		"",
	}, "\n")

	out, stripped, err := RedactGitConfigSecrets([]byte(in))
	if err != nil {
		t.Fatalf("RedactGitConfigSecrets: %v", err)
	}
	outStr := string(out)

	// The userinfo substring must not survive anywhere — the output OR the stripped
	// key names (noted key names are redacted too).
	if strings.Contains(outStr, "u:p%zz") || strings.Contains(strings.Join(stripped, "\n"), "u:p%zz") {
		t.Errorf("redacted config/stripped still contains the userinfo substring:\n%s\nstripped=%v", outStr, stripped)
	}

	// Both dropped keys must be recorded in stripped with the subsection REDACTED
	// (the Apple refusal keys on a non-empty stripped list; userinfo is kept out of
	// the names).
	for _, want := range []string{
		"url.<redacted>.insteadof",
		"http.<redacted>.sslverify",
	} {
		found := false
		for _, s := range stripped {
			if s == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in stripped keys, got %v", want, stripped)
		}
	}

	// The sibling key with a clean base is kept (in its serialized form: names are
	// lower-cased and values are always quoted).
	if !strings.Contains(outStr, "[http \"https://clean.example\"]\n\tsslverify = \"false\"\n") {
		t.Errorf("sibling key with a clean base was not kept:\n%s", outStr)
	}

	if _, err := parseGitConfigFile(out); err != nil {
		t.Errorf("redacted config does not re-parse: %v\n%s", err, outStr)
	}
}

// TestRedactGitConfigSecrets_ProxyHelperAndKeyNameForms covers proxy URL keys,
// sendemail.smtppass, lfs.<base> subsections, a remote-helper insteadOf base, and an
// includeIf hasconfig:remote.*.url subsection, plus the redacted key names. Synthetic
// placeholder userinfo only.
func TestRedactGitConfigSecrets_ProxyHelperAndKeyNameForms(t *testing.T) {
	in := strings.Join([]string{
		"[http]",
		"\tproxy = https://pxu:pxp@proxy.example/",
		"[http \"https://site.example/\"]",
		"\tproxy = https://pxu:pxp@proxy.example/",
		"[remote \"origin\"]",
		"\tproxy = https://pxu:pxp@proxy.example/",
		"\turl = https://clean.example/r.git",
		"[sendemail]",
		"\tsmtppass = smtpAphrase",
		"[sendemail \"work\"]",
		"\tsmtppass = smtpBphrase",
		"[lfs \"https://lfu:lfp@lfs.example/r.git/info/lfs\"]",
		"\taccess = basic",
		"[url \"http::https://iou:iop@up.example/\"]",
		"\tinsteadOf = https://up.example/",
		"[includeIf \"hasconfig:remote.*.url:https://icu:icp@ic.example/**\"]",
		"\tpath = /x/.gitconfig",
		"",
	}, "\n")

	out, stripped, err := RedactGitConfigSecrets([]byte(in))
	if err != nil {
		t.Fatalf("RedactGitConfigSecrets: %v", err)
	}
	outStr := string(out)
	joined := strings.Join(stripped, "\n")

	// No placeholder userinfo may survive in the output OR in the stripped names.
	for _, bad := range []string{"pxu:pxp", "smtpAphrase", "smtpBphrase", "lfu:lfp", "iou:iop", "icu:icp"} {
		if strings.Contains(outStr, bad) || strings.Contains(joined, bad) {
			t.Errorf("placeholder %q survived:\noutput:\n%s\nstripped=%v", bad, outStr, stripped)
		}
	}

	// Proxy keys sanitized + noted; sendemail passwords dropped + noted.
	for _, want := range []string{
		"http.proxy",
		"http.https://site.example/.proxy",
		"remote.origin.proxy",
		"sendemail.smtppass",
		"sendemail.work.smtppass",
	} {
		found := false
		for _, s := range stripped {
			if s == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in stripped, got %v", want, stripped)
		}
	}

	// Cleaned forms present in the serialized output.
	for _, keep := range []string{
		"\tproxy = \"https://proxy.example/\"\n",
		"[lfs \"https://lfs.example/r.git/info/lfs\"]\n",
		"[url \"http::https://up.example/\"]\n",
		"[includeif \"hasconfig:remote.*.url:https://ic.example/**\"]\n",
	} {
		if !strings.Contains(outStr, keep) {
			t.Errorf("expected cleaned form %q in output:\n%s", keep, outStr)
		}
	}
	// The sendemail passwords must be gone entirely.
	if strings.Contains(outStr, "smtppass") {
		t.Errorf("sendemail smtppass not dropped:\n%s", outStr)
	}

	if _, err := parseGitConfigFile(out); err != nil {
		t.Errorf("redacted config does not re-parse: %v\n%s", err, outStr)
	}
}

// TestRedactGitConfigSecrets_SchemelessProxyDropped checks that a schemeless proxy
// value user:password@host is dropped on both http.proxy and remote.<name>.proxy,
// and that a username-only proxy value is kept. Synthetic placeholders only.
func TestRedactGitConfigSecrets_SchemelessProxyDropped(t *testing.T) {
	// Each of these (value before the last "@" contains ":") must be dropped on both
	// proxy keys. The CORP\u case exercises a non-scheme username character.
	dropVals := []string{
		"u:p@proxy.example:8080",
		"john_doe:pw@proxy.example:8080",
		"u%40corp.example:pw@proxy.example:8080",
		"1u:pw@proxy.example:8080",
		`CORP\u:pw@proxy.example:8080`,
	}
	for _, v := range dropVals {
		cfgVal := strings.ReplaceAll(v, `\`, `\\`) // literal backslash in a git config value
		in := strings.Join([]string{
			"[http]",
			"\tproxy = " + cfgVal,
			"[remote \"origin\"]",
			"\tproxy = " + cfgVal,
			"",
		}, "\n")
		out, stripped, err := RedactGitConfigSecrets([]byte(in))
		if err != nil {
			t.Fatalf("RedactGitConfigSecrets(%q): %v", v, err)
		}
		outStr := string(out)
		if strings.Contains(outStr, "proxy.example") || strings.Contains(outStr, "@") {
			t.Errorf("proxy value %q not dropped from output:\n%s", v, outStr)
		}
		for _, want := range []string{"http.proxy", "remote.origin.proxy"} {
			found := false
			for _, s := range stripped {
				if s == want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("value %q: expected %q in stripped, got %v", v, want, stripped)
			}
		}
		if strings.Contains(strings.Join(stripped, "\n"), "@") {
			t.Errorf("value %q: stripped names contain userinfo: %v", v, stripped)
		}
	}

	// Username-only proxy value (no ":" before the last "@") is kept, nothing noted.
	in := strings.Join([]string{
		"[http]",
		"\tproxy = user@proxy.example:8080",
		"",
	}, "\n")
	out, stripped, err := RedactGitConfigSecrets([]byte(in))
	if err != nil {
		t.Fatalf("RedactGitConfigSecrets: %v", err)
	}
	if len(stripped) != 0 {
		t.Errorf("username-only proxy should note nothing, got %v", stripped)
	}
	if !strings.Contains(string(out), "proxy = \"user@proxy.example:8080\"") {
		t.Errorf("username-only proxy not kept:\n%s", string(out))
	}
}

// TestRedactGitConfigSecrets_ScpKeptAsRemoteAndBase is the regression for the removed
// opaque-"@" drop: an scp value host:repo@x.git (and git@h:o/r.git) is kept both as a
// remote url and as a url.<base> insteadOf base, and nothing is noted.
func TestRedactGitConfigSecrets_ScpKeptAsRemoteAndBase(t *testing.T) {
	for _, scp := range []string{"host:repo@x.git", "git@h:o/r.git"} {
		in := strings.Join([]string{
			"[remote \"origin\"]",
			"\turl = " + scp,
			"[url \"" + scp + "\"]",
			"\tinsteadOf = https://example/",
			"",
		}, "\n")
		out, stripped, err := RedactGitConfigSecrets([]byte(in))
		if err != nil {
			t.Fatalf("RedactGitConfigSecrets(%q): %v", scp, err)
		}
		outStr := string(out)
		if len(stripped) != 0 {
			t.Errorf("scp %q: expected nothing noted, got %v", scp, stripped)
		}
		if !strings.Contains(outStr, "url = \""+scp+"\"") {
			t.Errorf("scp %q: remote url not kept:\n%s", scp, outStr)
		}
		if !strings.Contains(outStr, "[url \""+scp+"\"]") {
			t.Errorf("scp %q: url base not kept:\n%s", scp, outStr)
		}
	}
}

// TestRedactGitConfigSecrets_IncludeIfHasconfigNamesRedacted asserts that the noted
// key name for an includeIf "hasconfig:remote.*.url:<url>" key never carries the
// <url>'s userinfo — the <url> is recorded as scheme://host/<redacted>, or as
// <redacted> when it does not parse as one — across an http(s) password, an ssh
// password, a remote-helper-prefixed url, and an unparseable url.
func TestRedactGitConfigSecrets_IncludeIfHasconfigNamesRedacted(t *testing.T) {
	type hasconfigCase struct{ placeholder, noted string }
	cases := map[string]hasconfigCase{ // <url> after the prefix -> placeholder that must not leak, expected noted name
		"https://iu:ip@ic.example/**":       {"iu:ip", "includeif.hasconfig:remote.*.url:https://ic.example/<redacted>.path"},
		"ssh://su:sp@sc.example/**":         {"su:sp", "includeif.hasconfig:remote.*.url:ssh://sc.example/<redacted>.path"},
		"http::https://hu:hp@hc.example/**": {"hu:hp", "includeif.hasconfig:remote.*.url:<redacted>.path"},
		"https://zu:zp%zz@zc.example/**":    {"zu:zp", "includeif.hasconfig:remote.*.url:<redacted>.path"},
	}
	for form, tc := range cases {
		placeholder := tc.placeholder
		in := strings.Join([]string{
			"[includeIf \"hasconfig:remote.*.url:" + form + "\"]",
			"\tpath = /x/.gitconfig",
			"",
		}, "\n")
		out, stripped, err := RedactGitConfigSecrets([]byte(in))
		if err != nil {
			t.Fatalf("form %q: RedactGitConfigSecrets: %v", form, err)
		}
		joined := strings.Join(stripped, "\n")
		foundRedacted := false
		for _, s := range stripped {
			if s == tc.noted {
				foundRedacted = true
				break
			}
		}
		if !foundRedacted {
			t.Errorf("includeIf hasconfig noted name: unexpected userinfo in output: form %q gave stripped=%v, want %s", form, stripped, tc.noted)
		}
		if strings.Contains(joined, placeholder) || strings.Contains(string(out), placeholder) {
			t.Errorf("includeIf hasconfig noted name: unexpected userinfo in output: placeholder %q present for form %q (stripped=%v)", placeholder, form, stripped)
		}
	}
}

// redactKeyCase is one row of the redaction key matrix. Each row is a config with a
// single variable. A positive row (noted set) carries a synthetic secret that must be
// scrubbed (want is the cleaned value) or dropped (want ""); a negative row (noted "")
// carries a benign value, or a sibling key of a dropped key, that must survive byte for
// byte with nothing noted.
type redactKeyCase struct {
	name   string           // subtest name
	rule   gitConfigKeyRule // the policy key the row covers
	header string           // section header line, e.g. `[submodule "lib"]`
	key    string           // variable name as written in the config
	value  string           // value as written (no characters that need escaping)
	want   string           // value after redaction; "" means the key is dropped
	noted  string           // expected stripped name; "" means a negative row
	leak   string           // synthetic secret that must survive nowhere ("" for negative rows)
}

// redactKeyMatrix covers every key in gitConfigDropRules, gitConfigURLRules and
// gitConfigRemoteRefRules with at least one positive and one negative row
// (TestRedactGitConfigSecrets_KeyMatrixCoversPolicy enforces this). Synthetic
// placeholder userinfo only.
var redactKeyMatrix = []redactKeyCase{
	// gitConfigDropRules: dropped entirely, with or without a subsection; the negative
	// rows are sibling keys that must be kept.
	{"credential_helper_dropped", gitConfigKeyRule{"credential", "*"}, "[credential]", "helper", "store --file /home/cu/.cred-ck1", "", "credential.helper", "cred-ck1"},
	{"credential_url_username_dropped", gitConfigKeyRule{"credential", "*"}, `[credential "https://cred.example"]`, "username", "cred-ck2", "", "credential.<redacted>.username", "cred-ck2"},
	{"credential_sibling_http_kept", gitConfigKeyRule{"credential", "*"}, `[http "https://cred.example"]`, "sslVerify", "true", "true", "", ""},
	{"http_extraheader_dropped", gitConfigKeyRule{"http", "extraheader"}, `[http "https://h.example"]`, "extraHeader", "Authorization: Basic eh-ck1", "", "http.https://h.example.extraheader", "eh-ck1"},
	{"http_extraheader_nosub_dropped", gitConfigKeyRule{"http", "extraheader"}, "[http]", "extraHeader", "Authorization: Bearer eh-ck2", "", "http.extraheader", "eh-ck2"},
	{"http_extraheader_sibling_kept", gitConfigKeyRule{"http", "extraheader"}, `[http "https://h.example"]`, "sslVerify", "false", "false", "", ""},
	{"http_cookiefile_dropped", gitConfigKeyRule{"http", "cookiefile"}, "[http]", "cookieFile", "/home/cu/.ck-cookies", "", "http.cookiefile", "ck-cookies"},
	{"http_cookiefile_sub_dropped", gitConfigKeyRule{"http", "cookiefile"}, `[http "https://h.example"]`, "cookieFile", "/home/cu/.ck-cookies2", "", "http.https://h.example.cookiefile", "ck-cookies2"},
	{"http_cookiefile_sibling_kept", gitConfigKeyRule{"http", "cookiefile"}, "[http]", "followRedirects", "true", "true", "", ""},
	{"http_sslkey_dropped", gitConfigKeyRule{"http", "sslkey"}, "[http]", "sslKey", "/home/cu/.ck-tlskey1.pem", "", "http.sslkey", "ck-tlskey1"},
	{"http_sslkey_sub_dropped", gitConfigKeyRule{"http", "sslkey"}, `[http "https://h.example"]`, "sslKey", "/home/cu/.ck-tlskey2.pem", "", "http.https://h.example.sslkey", "ck-tlskey2"},
	{"http_sslkey_sibling_sslcainfo_kept", gitConfigKeyRule{"http", "sslkey"}, "[http]", "sslCAInfo", "/etc/ssl/certs/ca-bundle.pem", "/etc/ssl/certs/ca-bundle.pem", "", ""},
	{"http_sslkey_sibling_sub_sslcainfo_kept", gitConfigKeyRule{"http", "sslkey"}, `[http "https://h.example"]`, "sslCAInfo", "/etc/ssl/certs/h-ca.pem", "/etc/ssl/certs/h-ca.pem", "", ""},
	{"http_sslcert_dropped", gitConfigKeyRule{"http", "sslcert"}, "[http]", "sslCert", "/home/cu/.ck-tlscert1.pem", "", "http.sslcert", "ck-tlscert1"},
	{"http_sslcert_sub_dropped", gitConfigKeyRule{"http", "sslcert"}, `[http "https://h.example"]`, "sslCert", "/home/cu/.ck-tlscert2.pem", "", "http.https://h.example.sslcert", "ck-tlscert2"},
	{"http_sslcert_sibling_sslcapath_kept", gitConfigKeyRule{"http", "sslcert"}, "[http]", "sslCAPath", "/etc/ssl/certs", "/etc/ssl/certs", "", ""},
	{"http_sslcert_sibling_sub_sslcapath_kept", gitConfigKeyRule{"http", "sslcert"}, `[http "https://h.example"]`, "sslCAPath", "/etc/ssl/h-certs", "/etc/ssl/h-certs", "", ""},
	{"sendemail_smtppass_dropped", gitConfigKeyRule{"sendemail", "smtppass"}, "[sendemail]", "smtpPass", "smtp-ck1", "", "sendemail.smtppass", "smtp-ck1"},
	{"sendemail_smtppass_sub_dropped", gitConfigKeyRule{"sendemail", "smtppass"}, `[sendemail "work"]`, "smtpPass", "smtp-ck2", "", "sendemail.work.smtppass", "smtp-ck2"},
	{"sendemail_smtpuser_kept", gitConfigKeyRule{"sendemail", "smtppass"}, "[sendemail]", "smtpUser", "sender@mail.example", "sender@mail.example", "", ""},
	{"imap_pass_dropped", gitConfigKeyRule{"imap", "pass"}, "[imap]", "pass", "imap-ck1", "", "imap.pass", "imap-ck1"},
	{"imap_pass_sub_dropped", gitConfigKeyRule{"imap", "pass"}, `[imap "work"]`, "pass", "imap-ck2", "", "imap.work.pass", "imap-ck2"},
	{"imap_user_kept", gitConfigKeyRule{"imap", "pass"}, "[imap]", "user", "imapuser", "imapuser", "", ""},
	{"imap_user_empty_kept", gitConfigKeyRule{"imap", "pass"}, "[imap]", "user", "", "", "", ""},
	{"imap_pass_mixedcase_header_dropped", gitConfigKeyRule{"imap", "pass"}, "[IMAP]", "pass", "imap-ck3", "", "imap.pass", "imap-ck3"},
	{"git_p4_password_dropped", gitConfigKeyRule{"git-p4", "password"}, "[git-p4]", "password", "p4-ck1", "", "git-p4.password", "p4-ck1"},
	{"git_p4_password_sub_dropped", gitConfigKeyRule{"git-p4", "password"}, `[git-p4 "depot"]`, "password", "p4-ck2", "", "git-p4.depot.password", "p4-ck2"},
	{"git_p4_user_kept", gitConfigKeyRule{"git-p4", "password"}, "[git-p4]", "user", "p4user", "p4user", "", ""},
	{"gitcvs_dbpass_dropped", gitConfigKeyRule{"gitcvs", "dbpass"}, "[gitcvs]", "dbPass", "cvs-ck1", "", "gitcvs.dbpass", "cvs-ck1"},
	{"gitcvs_ext_dbpass_dropped", gitConfigKeyRule{"gitcvs", "dbpass"}, `[gitcvs "ext"]`, "dbPass", "cvs-ck2", "", "gitcvs.ext.dbpass", "cvs-ck2"},
	{"gitcvs_dbuser_kept", gitConfigKeyRule{"gitcvs", "dbpass"}, "[gitcvs]", "dbUser", "cvsuser", "cvsuser", "", ""},

	// gitConfigURLRules: userinfo scrubbed (or the key dropped when it cannot be
	// parsed); scp forms, username-only non-http URLs and clean URLs kept.
	{"remote_url_scrubbed", gitConfigKeyRule{"remote", "url"}, `[remote "origin"]`, "url", "https://ru:rp-ck1@r.example/o/r.git", "https://r.example/o/r.git", "remote.origin.url", "rp-ck1"},
	{"remote_url_unparseable_dropped", gitConfigKeyRule{"remote", "url"}, `[remote "origin"]`, "url", "https://ru:rp-ck2%zz@r.example/o/r.git", "", "remote.origin.url", "rp-ck2"},
	{"remote_url_empty_password_scrubbed", gitConfigKeyRule{"remote", "url"}, `[remote "origin"]`, "url", "https://eu-ck1:@r.example/o/r.git", "https://r.example/o/r.git", "remote.origin.url", "eu-ck1"},
	{"remote_url_ipv6_host_scrubbed", gitConfigKeyRule{"remote", "url"}, `[remote "origin"]`, "url", "https://ru:rp-ck3@[::1]:8080/x", "https://[::1]:8080/x", "remote.origin.url", "rp-ck3"},
	{"remote_url_scp_kept", gitConfigKeyRule{"remote", "url"}, `[remote "origin"]`, "url", "git@r.example:o/r.git", "git@r.example:o/r.git", "", ""},
	{"remote_url_ssh_username_kept", gitConfigKeyRule{"remote", "url"}, `[remote "origin"]`, "url", "ssh://git@r.example/o/r.git", "ssh://git@r.example/o/r.git", "", ""},
	{"remote_pushurl_scrubbed", gitConfigKeyRule{"remote", "pushurl"}, `[remote "origin"]`, "pushurl", "https://pu:pp-ck1@r.example/o/r.git", "https://r.example/o/r.git", "remote.origin.pushurl", "pp-ck1"},
	{"remote_pushurl_scp_kept", gitConfigKeyRule{"remote", "pushurl"}, `[remote "origin"]`, "pushurl", "git@r.example:o/r.git", "git@r.example:o/r.git", "", ""},
	{"remote_lfsurl_scrubbed", gitConfigKeyRule{"remote", "lfsurl"}, `[remote "origin"]`, "lfsurl", "https://lu:lp-ck1@lfs.example/o/r", "https://lfs.example/o/r", "remote.origin.lfsurl", "lp-ck1"},
	{"remote_lfsurl_clean_kept", gitConfigKeyRule{"remote", "lfsurl"}, `[remote "origin"]`, "lfsurl", "https://lfs.example/o/r", "https://lfs.example/o/r", "", ""},
	{"remote_lfspushurl_scrubbed", gitConfigKeyRule{"remote", "lfspushurl"}, `[remote "origin"]`, "lfspushurl", "https://lu:lp-ck2@lfs.example/o/push", "https://lfs.example/o/push", "remote.origin.lfspushurl", "lp-ck2"},
	{"remote_lfspushurl_ssh_username_kept", gitConfigKeyRule{"remote", "lfspushurl"}, `[remote "origin"]`, "lfspushurl", "ssh://git@lfs.example/o/push", "ssh://git@lfs.example/o/push", "", ""},
	{"lfs_url_scrubbed", gitConfigKeyRule{"lfs", "url"}, "[lfs]", "url", "https://lu:lp-ck3@lfs.example/lfs", "https://lfs.example/lfs", "lfs.url", "lp-ck3"},
	{"lfs_url_clean_kept", gitConfigKeyRule{"lfs", "url"}, "[lfs]", "url", "https://lfs.example/lfs", "https://lfs.example/lfs", "", ""},
	{"lfs_pushurl_scrubbed", gitConfigKeyRule{"lfs", "pushurl"}, "[lfs]", "pushurl", "https://lu:lp-ck4@lfs.example/push", "https://lfs.example/push", "lfs.pushurl", "lp-ck4"},
	{"lfs_pushurl_ssh_username_kept", gitConfigKeyRule{"lfs", "pushurl"}, "[lfs]", "pushurl", "ssh://git@lfs.example/push", "ssh://git@lfs.example/push", "", ""},
	{"url_insteadof_scrubbed", gitConfigKeyRule{"url", "insteadof"}, `[url "https://mirror.example/"]`, "insteadOf", "https://iu:ip-ck1@up.example/", "https://up.example/", "url.https://mirror.example/.insteadof", "ip-ck1"},
	{"url_insteadof_scp_kept", gitConfigKeyRule{"url", "insteadof"}, `[url "https://mirror.example/"]`, "insteadOf", "git@up.example:", "git@up.example:", "", ""},
	{"url_pushinsteadof_scrubbed", gitConfigKeyRule{"url", "pushinsteadof"}, `[url "https://mirror.example/"]`, "pushInsteadOf", "https://iu:ip-ck2@up.example/", "https://up.example/", "url.https://mirror.example/.pushinsteadof", "ip-ck2"},
	{"url_pushinsteadof_ssh_username_kept", gitConfigKeyRule{"url", "pushinsteadof"}, `[url "https://mirror.example/"]`, "pushInsteadOf", "ssh://git@up.example/", "ssh://git@up.example/", "", ""},
	{"http_proxy_scrubbed", gitConfigKeyRule{"http", "proxy"}, "[http]", "proxy", "http://xu:xp-ck1@proxy.example:3128", "http://proxy.example:3128", "http.proxy", "xp-ck1"},
	{"http_proxy_schemeless_dropped", gitConfigKeyRule{"http", "proxy"}, `[http "https://site.example/"]`, "proxy", "xu:xp-ck2@proxy.example:3128", "", "http.https://site.example/.proxy", "xp-ck2"},
	{"http_proxy_username_kept", gitConfigKeyRule{"http", "proxy"}, "[http]", "proxy", "xuser@proxy.example:3128", "xuser@proxy.example:3128", "", ""},
	{"remote_proxy_scrubbed", gitConfigKeyRule{"remote", "proxy"}, `[remote "origin"]`, "proxy", "socks5://xu:xp-ck3@proxy.example:1080", "socks5://proxy.example:1080", "remote.origin.proxy", "xp-ck3"},
	{"remote_proxy_socks_username_kept", gitConfigKeyRule{"remote", "proxy"}, `[remote "origin"]`, "proxy", "socks5://xuser@proxy.example:1080", "socks5://xuser@proxy.example:1080", "", ""},
	{"submodule_url_scrubbed", gitConfigKeyRule{"submodule", "url"}, `[submodule "lib"]`, "url", "https://su:sp-ck1@sub.example/o/lib.git", "https://sub.example/o/lib.git", "submodule.lib.url", "sp-ck1"},
	{"submodule_url_unparseable_dropped", gitConfigKeyRule{"submodule", "url"}, `[submodule "lib"]`, "url", "https://su:sp-ck2%zz@sub.example/o/lib.git", "", "submodule.lib.url", "sp-ck2"},
	{"submodule_url_ssh_password_scrubbed", gitConfigKeyRule{"submodule", "url"}, `[submodule "lib"]`, "url", "ssh://su:sp-ck3@sub.example/o/lib.git", "ssh://sub.example/o/lib.git", "submodule.lib.url", "sp-ck3"},
	{"submodule_url_mixedcase_header_scrubbed", gitConfigKeyRule{"submodule", "url"}, `[Submodule "lib"]`, "url", "https://su:sp-ck4@sub.example/o/lib.git", "https://sub.example/o/lib.git", "submodule.lib.url", "sp-ck4"},
	{"submodule_url_scp_kept", gitConfigKeyRule{"submodule", "url"}, `[submodule "lib"]`, "url", "git@sub.example:o/lib.git", "git@sub.example:o/lib.git", "", ""},
	{"submodule_url_ssh_username_kept", gitConfigKeyRule{"submodule", "url"}, `[submodule "lib"]`, "url", "ssh://git@sub.example/o/lib.git", "ssh://git@sub.example/o/lib.git", "", ""},
	{"submodule_url_relative_kept", gitConfigKeyRule{"submodule", "url"}, `[submodule "lib"]`, "url", "../lib.git", "../lib.git", "", ""},
	{"submodule_url_empty_kept", gitConfigKeyRule{"submodule", "url"}, `[submodule "lib"]`, "url", "", "", "", ""},
	{"bundle_uri_scrubbed", gitConfigKeyRule{"bundle", "uri"}, `[bundle "base"]`, "uri", "https://du:dp-ck1@bundles.example/base.bundle", "https://bundles.example/base.bundle", "bundle.base.uri", "dp-ck1"},
	{"bundle_uri_unparseable_dropped", gitConfigKeyRule{"bundle", "uri"}, `[bundle "base"]`, "uri", "https://du:dp-ck2%zz@bundles.example/base.bundle", "", "bundle.base.uri", "dp-ck2"},
	{"bundle_uri_clean_kept", gitConfigKeyRule{"bundle", "uri"}, `[bundle "base"]`, "uri", "https://bundles.example/base.bundle", "https://bundles.example/base.bundle", "", ""},
	{"bundle_uri_path_kept", gitConfigKeyRule{"bundle", "uri"}, `[bundle "base"]`, "uri", "/srv/bundles/base.bundle", "/srv/bundles/base.bundle", "", ""},
	{"fetch_bundleuri_scrubbed", gitConfigKeyRule{"fetch", "bundleuri"}, "[fetch]", "bundleURI", "https://du:dp-ck3@bundles.example/list", "https://bundles.example/list", "fetch.bundleuri", "dp-ck3"},
	{"fetch_bundleuri_clean_kept", gitConfigKeyRule{"fetch", "bundleuri"}, "[fetch]", "bundleURI", "https://bundles.example/list", "https://bundles.example/list", "", ""},
	{"imap_host_url_scrubbed", gitConfigKeyRule{"imap", "host"}, "[imap]", "host", "imaps://mu:mp-ck1@imap.example", "imaps://imap.example", "imap.host", "mp-ck1"},
	{"imap_host_plain_url_scrubbed", gitConfigKeyRule{"imap", "host"}, "[imap]", "host", "imap://mu:mp-ck2@imap.example:143", "imap://imap.example:143", "imap.host", "mp-ck2"},
	{"imap_host_unparseable_dropped", gitConfigKeyRule{"imap", "host"}, "[imap]", "host", "imaps://mu:mp-ck3%zz@imap.example", "", "imap.host", "mp-ck3"},
	{"imap_host_bare_kept", gitConfigKeyRule{"imap", "host"}, "[imap]", "host", "imap.example", "imap.example", "", ""},
	{"imap_host_username_kept", gitConfigKeyRule{"imap", "host"}, "[imap]", "host", "imaps://muser@imap.example", "imaps://muser@imap.example", "", ""},
	{"svn_remote_url_scrubbed", gitConfigKeyRule{"svn-remote", "url"}, `[svn-remote "svn"]`, "url", "https://vu:vp-ck1@svn.example/repo", "https://svn.example/repo", "svn-remote.svn.url", "vp-ck1"},
	{"svn_remote_url_svnssh_password_scrubbed", gitConfigKeyRule{"svn-remote", "url"}, `[svn-remote "svn"]`, "url", "svn+ssh://vu:vp-ck2@svn.example/repo", "svn+ssh://svn.example/repo", "svn-remote.svn.url", "vp-ck2"},
	{"svn_remote_url_svnssh_username_kept", gitConfigKeyRule{"svn-remote", "url"}, `[svn-remote "svn"]`, "url", "svn+ssh://vuser@svn.example/repo", "svn+ssh://vuser@svn.example/repo", "", ""},
	{"svn_remote_pushurl_scrubbed", gitConfigKeyRule{"svn-remote", "pushurl"}, `[svn-remote "svn"]`, "pushurl", "https://vu:vp-ck3@svn.example/repo", "https://svn.example/repo", "svn-remote.svn.pushurl", "vp-ck3"},
	{"svn_remote_pushurl_clean_kept", gitConfigKeyRule{"svn-remote", "pushurl"}, `[svn-remote "svn"]`, "pushurl", "https://svn.example/repo", "https://svn.example/repo", "", ""},
	{"svn_remote_commiturl_scrubbed", gitConfigKeyRule{"svn-remote", "commiturl"}, `[svn-remote "svn"]`, "commiturl", "https://vu:vp-ck4@svn.example/repo", "https://svn.example/repo", "svn-remote.svn.commiturl", "vp-ck4"},
	{"svn_remote_commiturl_clean_kept", gitConfigKeyRule{"svn-remote", "commiturl"}, `[svn-remote "svn"]`, "commiturl", "https://svn.example/repo", "https://svn.example/repo", "", ""},
	{"svn_commiturl_scrubbed", gitConfigKeyRule{"svn", "commiturl"}, "[svn]", "commiturl", "https://vu:vp-ck5@svn.example/repo", "https://svn.example/repo", "svn.commiturl", "vp-ck5"},
	{"svn_commiturl_clean_kept", gitConfigKeyRule{"svn", "commiturl"}, "[svn]", "commiturl", "https://svn.example/repo", "https://svn.example/repo", "", ""},
	{"svn_remote_rewriteroot_scrubbed", gitConfigKeyRule{"svn-remote", "rewriteroot"}, `[svn-remote "svn"]`, "rewriteRoot", "https://vu:vp-ck6@svn.example/mirror", "https://svn.example/mirror", "svn-remote.svn.rewriteroot", "vp-ck6"},
	{"svn_remote_rewriteroot_clean_kept", gitConfigKeyRule{"svn-remote", "rewriteroot"}, `[svn-remote "svn"]`, "rewriteRoot", "https://svn.example/mirror", "https://svn.example/mirror", "", ""},

	// gitConfigRemoteRefRules: scrubbed only when the value is a URL or a remote-helper
	// form; a remote name, an scp host:path and a username-only URL are kept.
	{"branch_remote_url_scrubbed", gitConfigKeyRule{"branch", "remote"}, `[branch "main"]`, "remote", "https://bu:bp-ck1@b.example/o/r.git", "https://b.example/o/r.git", "branch.main.remote", "bp-ck1"},
	{"branch_remote_helper_scrubbed", gitConfigKeyRule{"branch", "remote"}, `[branch "main"]`, "remote", "http::https://bu:bp-ck2@b.example/o/r.git", "http::https://b.example/o/r.git", "branch.main.remote", "bp-ck2"},
	{"branch_remote_unparseable_dropped", gitConfigKeyRule{"branch", "remote"}, `[branch "main"]`, "remote", "https://bu:bp-ck3%zz@b.example/o/r.git", "", "branch.main.remote", "bp-ck3"},
	{"branch_remote_helper_clean_kept", gitConfigKeyRule{"branch", "remote"}, `[branch "main"]`, "remote", "http::https://b.example/o/r.git", "http::https://b.example/o/r.git", "", ""},
	{"branch_remote_name_kept", gitConfigKeyRule{"branch", "remote"}, `[branch "main"]`, "remote", "origin", "origin", "", ""},
	{"branch_remote_empty_kept", gitConfigKeyRule{"branch", "remote"}, `[branch "main"]`, "remote", "", "", "", ""},
	{"branch_remote_scp_kept", gitConfigKeyRule{"branch", "remote"}, `[branch "main"]`, "remote", "git@b.example:o/r.git", "git@b.example:o/r.git", "", ""},
	{"branch_remote_ssh_username_kept", gitConfigKeyRule{"branch", "remote"}, `[branch "main"]`, "remote", "ssh://git@b.example/o/r.git", "ssh://git@b.example/o/r.git", "", ""},
	{"branch_pushremote_url_scrubbed", gitConfigKeyRule{"branch", "pushremote"}, `[branch "main"]`, "pushRemote", "https://bu:bp-ck4@b.example/o/fork.git", "https://b.example/o/fork.git", "branch.main.pushremote", "bp-ck4"},
	{"branch_pushremote_name_kept", gitConfigKeyRule{"branch", "pushremote"}, `[branch "main"]`, "pushRemote", "fork", "fork", "", ""},
	{"remote_pushdefault_url_scrubbed", gitConfigKeyRule{"remote", "pushdefault"}, "[remote]", "pushDefault", "https://bu:bp-ck5@b.example/o/fork.git", "https://b.example/o/fork.git", "remote.pushdefault", "bp-ck5"},
	{"remote_pushdefault_name_kept", gitConfigKeyRule{"remote", "pushdefault"}, "[remote]", "pushDefault", "origin", "origin", "", ""},
}

// TestRedactGitConfigSecrets_KeyMatrix runs every row of redactKeyMatrix: a positive row
// must have its value scrubbed to want (or the key dropped) with exactly its noted name
// recorded and the secret surviving nowhere; a negative row must come back byte for
// byte with nothing noted.
func TestRedactGitConfigSecrets_KeyMatrix(t *testing.T) {
	for _, tc := range redactKeyMatrix {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.header + "\n\t" + tc.key + " = " + tc.value + "\n"
			inVars, err := parseGitConfigFile([]byte(in))
			if err != nil || len(inVars) != 1 {
				t.Fatalf("row input does not parse to one variable (%d, %v):\n%s", len(inVars), err, in)
			}
			out, stripped, err := RedactGitConfigSecrets([]byte(in))
			if err != nil {
				t.Fatalf("RedactGitConfigSecrets: %v", err)
			}
			outVars, err := parseGitConfigFile(out)
			if err != nil {
				t.Fatalf("redacted config does not re-parse: %v\n%s", err, out)
			}
			if tc.noted == "" {
				// Negative row: kept byte for byte, nothing noted.
				if len(stripped) != 0 {
					t.Errorf("benign value noted as stripped: %v", stripped)
				}
				if len(outVars) != 1 || !sameGitConfigKey(outVars[0], inVars[0]) || outVars[0].Value != inVars[0].Value {
					t.Fatalf("benign variable not kept unchanged: got %+v, want %+v", outVars, inVars[0])
				}
				if outVars[0].Value != tc.want {
					t.Errorf("kept value = %q, want %q", outVars[0].Value, tc.want)
				}
				if string(out) != string(serializeGitConfig(inVars)) {
					t.Errorf("output not byte-equal to the input re-serialised:\n%s", out)
				}
				return
			}
			// Positive row: exactly the noted name recorded, and the secret gone.
			if len(stripped) != 1 || stripped[0] != tc.noted {
				t.Errorf("stripped = %v, want [%s]", stripped, tc.noted)
			}
			if tc.leak == "" {
				t.Fatal("positive row without a leak marker")
			}
			if !strings.Contains(tc.value, tc.leak) {
				t.Fatalf("leak marker %q does not occur in the row's value, so its absence from the output proves nothing:\n%s", tc.leak, in)
			}
			if strings.Contains(string(out), tc.leak) || strings.Contains(strings.Join(stripped, "\n"), tc.leak) {
				t.Errorf("secret %q survived:\noutput:\n%s\nstripped=%v", tc.leak, out, stripped)
			}
			if tc.want == "" {
				if len(outVars) != 0 {
					t.Errorf("key not dropped: %+v", outVars)
				}
				return
			}
			if len(outVars) != 1 {
				t.Fatalf("scrubbed key missing from output:\n%s", out)
			}
			got := outVars[0]
			if got.Value != tc.want {
				t.Errorf("scrubbed value = %q, want %q", got.Value, tc.want)
			}
			if !sameGitConfigKey(got, inVars[0]) {
				t.Errorf("scrubbed key moved: got %+v, want key of %+v", got, inVars[0])
			}
		})
	}
}

// sameGitConfigKey reports whether a and b name the same key (section, subsection and
// variable name); values and line numbers are not compared.
func sameGitConfigKey(a, b gitConfigVar) bool {
	return a.Section == b.Section && a.Subsection == b.Subsection && a.HasSubsection == b.HasSubsection && a.Name == b.Name
}

// TestRedactGitConfigSecrets_KeyMatrixCoversPolicy is the exhaustiveness guard. It
// enumerates the policy tables (gitConfigDropRules, gitConfigURLRules,
// gitConfigRemoteRefRules) and fails if a key is in the code but has no positive and
// negative row in redactKeyMatrix, or if a row names a key that is not in the code.
// Adding a key to the policy without matrix rows therefore fails the test suite.
func TestRedactGitConfigSecrets_KeyMatrixCoversPolicy(t *testing.T) {
	policy := map[gitConfigKeyRule]string{}
	for kind, rules := range map[string][]gitConfigKeyRule{
		"drop":       gitConfigDropRules,
		"url":        gitConfigURLRules,
		"remote-ref": gitConfigRemoteRefRules,
	} {
		if len(rules) == 0 {
			t.Errorf("policy table %q is empty", kind)
		}
		for _, r := range rules {
			if r.Section != strings.ToLower(r.Section) || r.Name != strings.ToLower(r.Name) || r.Section == "" || r.Name == "" {
				t.Errorf("policy rule %+v (%s) is not a lower-cased section and name", r, kind)
			}
			if prev, dup := policy[r]; dup {
				t.Errorf("policy rule %+v is in both %q and %q", r, prev, kind)
			}
			policy[r] = kind
		}
	}
	type cover struct{ pos, neg int }
	covered := map[gitConfigKeyRule]*cover{}
	names := map[string]bool{}
	for _, tc := range redactKeyMatrix {
		if names[tc.name] {
			t.Errorf("duplicate matrix row name %q", tc.name)
		}
		names[tc.name] = true
		kind, ok := policy[tc.rule]
		if !ok {
			t.Errorf("matrix row %q names %+v, which is not in the policy tables", tc.name, tc.rule)
			continue
		}
		c := covered[tc.rule]
		if c == nil {
			c = &cover{}
			covered[tc.rule] = c
		}
		vars, err := parseGitConfigFile([]byte(tc.header + "\n\t" + tc.key + " = " + tc.value + "\n"))
		if err != nil || len(vars) != 1 {
			t.Errorf("matrix row %q does not parse to one variable", tc.name)
			continue
		}
		hits := tc.rule.matches(vars[0])
		if tc.noted == "" {
			c.neg++
			// A negative row of a URL or remote-ref rule must use that rule's own key
			// with a benign value; a negative row of a drop rule must be a sibling key
			// the rule does not match.
			if kind != "drop" && !hits {
				t.Errorf("negative matrix row %q does not use the key of %+v", tc.name, tc.rule)
			}
			if kind == "drop" && hits {
				t.Errorf("negative matrix row %q of drop rule %+v uses a dropped key", tc.name, tc.rule)
			}
			continue
		}
		c.pos++
		// A positive row must exercise the rule itself, and a drop rule must drop.
		if !hits {
			t.Errorf("positive matrix row %q does not exercise %+v", tc.name, tc.rule)
		}
		if kind == "drop" && tc.want != "" {
			t.Errorf("positive row %q of drop rule %+v must expect the key dropped", tc.name, tc.rule)
		}
	}
	for r, kind := range policy {
		c := covered[r]
		if c == nil || c.pos == 0 || c.neg == 0 {
			t.Errorf("policy %s rule %+v needs at least one positive and one negative matrix row (have %+v)", kind, r, c)
		}
	}
}
