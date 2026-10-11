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

package cmd

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func fileMode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	return fi.Mode().Perm()
}

// Slugs contain no dot, so project a-b / agent c and project a / agent b-c
// get different Host names.
func TestSSHHostName_NoCollision(t *testing.T) {
	assert.Equal(t, "scion.hub.a-b.c", sshHostName("hub", "a-b", "c"))
	assert.Equal(t, "scion.hub.a.b-c", sshHostName("hub", "a", "b-c"))
	assert.NotEqual(t, sshHostName("hub", "a-b", "c"), sshHostName("hub", "a", "b-c"))
}

func TestSSHPaths(t *testing.T) {
	p := sshPaths{home: "/h"}
	assert.Equal(t, "/h/.ssh/config", p.userConfig())
	assert.Equal(t, "/h/.ssh/config.d/scion-hub", p.includeFile("hub"))
	assert.Equal(t, "/h/.scion/ssh/known_hosts-hub", p.knownHostsFile("hub"))
	assert.Equal(t, "~/.scion/ssh/known_hosts-hub", sshKnownHostsRef("hub"))
}

// The hub label needs an http or https endpoint with a host.
func TestSSHEndpointURL(t *testing.T) {
	u, err := sshEndpointURL("https://hub.example.com:8443")
	require.NoError(t, err)
	assert.Equal(t, "hub.example.com", u.Hostname())
	assert.Equal(t, "8443", u.Port())
	for _, bad := range []string{"", "hub.example.com", "ftp://hub", "https://"} {
		_, err := sshEndpointURL(bad)
		assert.Error(t, err, bad)
	}
}

// Label candidates for normalized endpoints: the first DNS label, or the
// whole host (with a non-default port) for an IP address or localhost.
func TestSSHHubLabelCandidates(t *testing.T) {
	cases := []struct{ endpoint, preferred, fallback string }{
		{"https://hub.example.com", "hub", "hub-example-com"},
		{"https://hub.example.com:8443", "hub", "hub-example-com-8443"},
		{"https://hub.example.com:443", "hub", "hub-example-com"},
		{"http://hub.example.com:80", "hub", "hub-example-com"},
		{"http://hub.example.com:443", "hub", "hub-example-com-443"},
		{"https://127.0.0.1:443", "127-0-0-1", "127-0-0-1"},
		{"http://localhost:80", "localhost", "localhost"},
		{"https://Hub-1.example.com", "hub-1", "hub-1-example-com"},
		{"http://127.0.0.1:8080", "127-0-0-1-8080", "127-0-0-1-8080"},
		{"https://10.0.0.5", "10-0-0-5", "10-0-0-5"},
		{"http://localhost:9810", "localhost-9810", "localhost-9810"},
		{"https://localhost", "localhost", "localhost"},
	}
	for _, c := range cases {
		u, err := sshEndpointURL(c.endpoint)
		require.NoError(t, err, c.endpoint)
		pref, fb := sshHubLabelCandidates(u)
		assert.Equal(t, c.preferred, pref, c.endpoint)
		assert.Equal(t, c.fallback, fb, c.endpoint)
	}
}

// Label collision: two endpoints share a first DNS label, so the second
// gets the full host-and-port slug.
func TestResolveSSHHubLabel_CollisionUsesFullHostPort(t *testing.T) {
	u1, err := sshEndpointURL("https://hub.example.com")
	require.NoError(t, err)
	l1, err := resolveSSHHubLabel("https://hub.example.com", u1, nil)
	require.NoError(t, err)
	assert.Equal(t, "hub", l1)

	files := []sshIncludeFileInfo{{path: "/h/.ssh/config.d/scion-hub", label: "hub", endpoint: "https://hub.example.com"}}
	u2, err := sshEndpointURL("https://hub.other.org:8443")
	require.NoError(t, err)
	l2, err := resolveSSHHubLabel("https://hub.other.org:8443", u2, files)
	require.NoError(t, err)
	assert.Equal(t, "hub-other-org-8443", l2)

	// A file holding the label without a header also blocks it.
	bare := []sshIncludeFileInfo{{path: "/h/.ssh/config.d/scion-hub", label: "hub"}}
	l3, err := resolveSSHHubLabel("https://hub.other.org:8443", u2, bare)
	require.NoError(t, err)
	assert.Equal(t, "hub-other-org-8443", l3)

	// Both candidates taken is an error.
	both := append(files, sshIncludeFileInfo{path: "/h/.ssh/config.d/scion-hub-other-org-8443", label: "hub-other-org-8443", endpoint: "https://x"})
	_, err = resolveSSHHubLabel("https://hub.other.org:8443", u2, both)
	assert.Error(t, err)
}

// A label recorded for the same endpoint is reused even when it differs
// from the label that would be derived now.
func TestResolveSSHHubLabel_ReusesRecordedLabel(t *testing.T) {
	u, err := sshEndpointURL("https://hub.example.com")
	require.NoError(t, err)
	files := []sshIncludeFileInfo{{path: "/h/.ssh/config.d/scion-myhub", label: "myhub", endpoint: "https://hub.example.com"}}
	l, err := resolveSSHHubLabel("https://hub.example.com", u, files)
	require.NoError(t, err)
	assert.Equal(t, "myhub", l)
}

// The header records endpoint and label; listing reads them back, takes the
// label from the file name when there is no header, skips temporary files
// and treats a missing directory as empty.
func TestListSSHIncludeFiles(t *testing.T) {
	home := t.TempDir()
	p := sshPaths{home: home}
	files, err := listSSHIncludeFiles(p)
	require.NoError(t, err)
	assert.Empty(t, files)

	require.NoError(t, ensureSSHDir(home, p.includeDir()))
	require.NoError(t, os.WriteFile(p.includeFile("myhub"), []byte(renderSSHIncludeFile("https://hub.example.com", "myhub", nil)), 0o600))
	require.NoError(t, os.WriteFile(p.includeFile("bare"), []byte("Host x\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(p.includeDir(), sshTempPrefix+"scion-myhub-123"), []byte("partial"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(p.includeDir(), "other"), []byte("Host y\n"), 0o600))

	files, err = listSSHIncludeFiles(p)
	require.NoError(t, err)
	assert.Equal(t, []sshIncludeFileInfo{
		{path: p.includeFile("bare"), label: "bare"},
		{path: p.includeFile("myhub"), label: "myhub", endpoint: "https://hub.example.com"},
	}, files)
}

func TestParseSSHIncludeHeader(t *testing.T) {
	ep, label := parseSSHIncludeHeader(renderSSHIncludeFile("https://hub.example.com", "hub", map[string]string{"scion.hub.p.a": "Host scion.hub.p.a\n"}))
	assert.Equal(t, "https://hub.example.com", ep)
	assert.Equal(t, "hub", label)
	ep, label = parseSSHIncludeHeader("Host x\n" + sshHeaderEndpointKey + "https://late\n")
	assert.Empty(t, ep, "only the leading comment block is the header")
	assert.Empty(t, label)
}

// New directories, parents included, are 0700; an existing one keeps its
// mode.
func TestEnsureSSHDir(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(home, ".ssh"), 0o755))
	require.NoError(t, os.Chmod(filepath.Join(home, ".ssh"), 0o755))
	require.NoError(t, ensureSSHDir(home, filepath.Join(home, ".ssh", "config.d")))
	require.NoError(t, ensureSSHDir(home, filepath.Join(home, ".scion", "ssh")))
	assert.Equal(t, fs.FileMode(0o755), fileMode(t, filepath.Join(home, ".ssh")))
	assert.Equal(t, fs.FileMode(0o700), fileMode(t, filepath.Join(home, ".ssh", "config.d")))
	assert.Equal(t, fs.FileMode(0o700), fileMode(t, filepath.Join(home, ".scion")))
	assert.Equal(t, fs.FileMode(0o700), fileMode(t, filepath.Join(home, ".scion", "ssh")))
	assert.Error(t, ensureSSHDir(home, "/elsewhere"))
}

// writeFileAtomic replaces the file through a rename (a new inode), gives a
// new file the requested mode, keeps an existing mode and leaves no
// temporary file behind.
func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	require.NoError(t, writeFileAtomic(path, []byte("one"), 0o600))
	assert.Equal(t, fs.FileMode(0o600), fileMode(t, path))
	require.NoError(t, os.Chmod(path, 0o640))
	before, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, writeFileAtomic(path, []byte("two"), 0o600))
	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.False(t, os.SameFile(before, after), "the file is replaced by rename, not rewritten in place")
	assert.Equal(t, "two", readTestFile(t, path))
	assert.Equal(t, fs.FileMode(0o640), after.Mode().Perm())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	// A failed write (directory missing) reports the error and leaves nothing.
	assert.Error(t, writeFileAtomic(filepath.Join(dir, "missing", "f"), []byte("x"), 0o600))
}

// A failure after the temporary file exists (the rename onto a non-empty
// directory fails) returns the error and removes the temporary file.
func TestWriteFileAtomic_RemovesTempFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.Mkdir(target, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(target, "keep"), []byte("x"), 0o600))

	assert.Error(t, writeFileAtomic(target, []byte("data"), 0o600))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, strings.HasPrefix(e.Name(), sshTempPrefix), "temporary file left behind: %s", e.Name())
	}
	assert.Len(t, entries, 1)
	assert.Equal(t, "x", readTestFile(t, filepath.Join(target, "keep")))
}

// A symlink stays a symlink; its target is replaced and keeps its mode.
func TestWriteFileAtomic_Symlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "real")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o640))
	require.NoError(t, os.Chmod(target, 0o640))
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(target, link))

	require.NoError(t, writeFileAtomic(link, []byte("new"), 0o600))
	fi, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, fi.Mode()&fs.ModeSymlink)
	assert.Equal(t, "new", readTestFile(t, target))
	assert.Equal(t, fs.FileMode(0o640), fileMode(t, target))
}

// The Include line goes at the top once: a new ~/.ssh/config is 0600, an
// existing one keeps its mode and content, a second call changes nothing,
// and a line inside a Host block does not count.
func TestEnsureSSHIncludeLine(t *testing.T) {
	t.Run("new config", func(t *testing.T) {
		home := t.TempDir()
		p := sshPaths{home: home}
		changed, err := ensureSSHIncludeLine(p)
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, sshIncludeLine+"\n", readTestFile(t, p.userConfig()))
		assert.Equal(t, fs.FileMode(0o600), fileMode(t, p.userConfig()))
		assert.Equal(t, fs.FileMode(0o700), fileMode(t, p.sshDir()))

		changed, err = ensureSSHIncludeLine(p)
		require.NoError(t, err)
		assert.False(t, changed)
		assert.Equal(t, sshIncludeLine+"\n", readTestFile(t, p.userConfig()))
	})
	t.Run("existing config keeps mode and content", func(t *testing.T) {
		home := t.TempDir()
		p := sshPaths{home: home}
		require.NoError(t, os.MkdirAll(p.sshDir(), 0o700))
		original := "Host example\n  User someone\n"
		require.NoError(t, os.WriteFile(p.userConfig(), []byte(original), 0o644))
		require.NoError(t, os.Chmod(p.userConfig(), 0o644))
		_, err := ensureSSHIncludeLine(p)
		require.NoError(t, err)
		assert.Equal(t, sshIncludeLine+"\n\n"+original, readTestFile(t, p.userConfig()))
		assert.Equal(t, fs.FileMode(0o644), fileMode(t, p.userConfig()))
	})
	t.Run("line inside a Host block", func(t *testing.T) {
		home := t.TempDir()
		p := sshPaths{home: home}
		require.NoError(t, os.MkdirAll(p.sshDir(), 0o700))
		original := "Host a\n  " + sshIncludeLine + "\n"
		require.NoError(t, os.WriteFile(p.userConfig(), []byte(original), 0o600))
		changed, err := ensureSSHIncludeLine(p)
		require.NoError(t, err)
		assert.True(t, changed)
		assert.True(t, strings.HasPrefix(readTestFile(t, p.userConfig()), sshIncludeLine+"\n"))
	})
	assert.True(t, hasSSHIncludeLine("# comment\n"+sshIncludeLine+"\nHost a\n"))
	assert.True(t, hasSSHIncludeLine("Include   ~/.ssh/config.d/scion-*\n"))
	assert.True(t, hasSSHIncludeLine("include ~/.ssh/config.d/scion-*\n"))
	assert.True(t, hasSSHIncludeLine("INCLUDE ~/.ssh/config.d/scion-*\n"))
	assert.False(t, hasSSHIncludeLine("Include ~/.ssh/config.d/scion-* ~/.ssh/other\n"))
	assert.False(t, hasSSHIncludeLine("Include ~/.ssh/config.d/SCION-*\n"))
	assert.False(t, hasSSHIncludeLine("Match all\n"+sshIncludeLine+"\n"))
	assert.False(t, hasSSHIncludeLine("host a\n"+sshIncludeLine+"\n"))
	assert.False(t, hasSSHIncludeLine(""))
}

func TestKnownHostsParseAndRender(t *testing.T) {
	content := "# comment\n\nscion.h.p.b ssh-ed25519 BBBB\nscion.h.p.a ssh-ed25519 AAAA extra\nmalformed\n"
	keys := parseKnownHosts(content)
	assert.Equal(t, map[string]string{"scion.h.p.a": "ssh-ed25519 AAAA", "scion.h.p.b": "ssh-ed25519 BBBB"}, keys)
	assert.Equal(t, "scion.h.p.a ssh-ed25519 AAAA\nscion.h.p.b ssh-ed25519 BBBB\n", renderKnownHosts(keys))

	missing, err := readKnownHosts(filepath.Join(t.TempDir(), "none"))
	require.NoError(t, err)
	assert.Empty(t, missing)
}

// Blocks survive a render and parse round trip, sorted by host name, with
// the header first.
func TestSSHIncludeFileRoundTrip(t *testing.T) {
	blocks := map[string]string{
		"scion.h.p.b": renderSSHHostBlock("/bin/scion", "https://h.example", "h", "p", "b"),
		"scion.h.p.a": renderSSHHostBlock("/bin/scion", "https://h.example", "h", "p", "a"),
	}
	content := renderSSHIncludeFile("https://h.example", "h", blocks)
	assert.True(t, strings.HasPrefix(content, sshHeaderGeneratedTag+"\n"+sshHeaderEndpointKey+"https://h.example\n"+sshHeaderLabelKey+"h\n\nHost scion.h.p.a\n"))
	assert.Less(t, strings.Index(content, "Host scion.h.p.a"), strings.Index(content, "Host scion.h.p.b"))
	assert.Equal(t, blocks, parseSSHHostBlocks(content))
}

// The Host block carries every line the design lists, with the absolute
// scion path quoted for the shell and the hub and project pinned.
func TestRenderSSHHostBlock(t *testing.T) {
	got := renderSSHHostBlock("/Applications/My Tools/scion", "https://hub.example.com:8443", "hub", "a-b", "c")
	want := "Host scion.hub.a-b.c\n" +
		"  ProxyCommand '/Applications/My Tools/scion' tunnel --stdio --ssh --hub https://hub.example.com:8443 --project a-b c\n" +
		"  HostKeyAlias scion.hub.a-b.c\n" +
		"  UserKnownHostsFile ~/.scion/ssh/known_hosts-hub\n" +
		"  StrictHostKeyChecking yes\n"
	assert.Equal(t, want, got)
}

func TestSSHShellQuote(t *testing.T) {
	assert.Equal(t, "/usr/local/bin/scion", sshShellQuote("/usr/local/bin/scion"))
	assert.Equal(t, "https://hub.example.com:8443", sshShellQuote("https://hub.example.com:8443"))
	assert.Equal(t, "'/Applications/My Tools/scion'", sshShellQuote("/Applications/My Tools/scion"))
	assert.Equal(t, "/opt/100%%/scion", sshShellQuote("/opt/100%/scion"))
	assert.Equal(t, `'/it'\''s/scion'`, sshShellQuote("/it's/scion"))
}
