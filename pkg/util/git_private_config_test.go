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

	wantStripped := []string{
		"credential.helper",
		"credential.https://host.example.username",
		"http.https://host.example.extraheader",
		"remote.origin.url",
		"url.https://bob:pw@mirror.example/.insteadof",
	}
	got := append([]string(nil), stripped...)
	sort.Strings(got)
	sort.Strings(wantStripped)
	if strings.Join(got, "\n") != strings.Join(wantStripped, "\n") {
		t.Errorf("stripped keys = %v, want %v", got, wantStripped)
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
		"ssh://u@h/x":     {"ssh://u@h/x", false, false},   // not http(s), userinfo kept
		// Unparseable but URL-shaped ("://") and carries userinfo (an invalid percent
		// escape in the userinfo): drop the key so the userinfo cannot survive.
		"https://u:p%zz@host/x": {"", false, true},
		// Unparseable, URL-shaped, but no userinfo: kept unchanged.
		"https://host/x%zz": {"https://host/x%zz", false, false},
		// Unparseable scp-style base with userinfo but no "://": kept unchanged.
		"git@github.com:o/r.git": {"git@github.com:o/r.git", false, false},
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

	// The userinfo substring must not survive anywhere, section header included.
	if strings.Contains(outStr, "u:p%zz") {
		t.Errorf("redacted config still contains the userinfo substring:\n%s", outStr)
	}

	// Both dropped keys must be recorded in stripped (the Apple refusal keys on a
	// non-empty stripped list).
	for _, want := range []string{
		"url.https://u:p%zz@host.example.insteadof",
		"http.https://u:p%zz@host.example.sslverify",
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
