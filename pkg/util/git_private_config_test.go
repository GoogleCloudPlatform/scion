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
	// "<redacted>", and a url.<base> base with userinfo appears in its cleaned form.
	wantStripped := []string{
		"credential.helper",
		"credential.<redacted>.username",
		"http.https://host.example.extraheader",
		"remote.origin.url",
		"url.https://mirror.example/.insteadof",
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
		// ftp/ftps: userinfo removed entirely.
		"ftp://u:p@h/x": {"ftp://h/x", true, false},
		"ftps://u@h/x":  {"ftps://h/x", true, false},
		// other schemes with a password: keep only the username.
		"ssh://u:p@h/x":       {"ssh://u@h/x", true, false},
		"socks5://u:p@h:1080": {"socks5://u@h:1080", true, false},
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
