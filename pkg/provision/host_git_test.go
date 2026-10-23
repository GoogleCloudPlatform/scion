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

package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckHostCloneURL(t *testing.T) {
	accepted := []string{
		"https://github.com/org/repo.git",
		"https://user@git.example.com:8443/org/repo",
		"ssh://git@github.com/org/repo.git",
		"ssh://git@[2001:db8::1]:22/org/repo.git",
		"git://git.example.com/org/repo.git",
		"git@github.com:org/repo.git",
		"github.com:org/repo.git",
		"git@[2001:db8::1]:org/repo.git",
	}
	for _, u := range accepted {
		assert.NoError(t, CheckHostCloneURL(u), u)
	}
	refused := []string{
		"",
		"/srv/repos/repo.git",
		"./repo",
		"../repo",
		"repo",
		"~/repo.git",
		"C:/repos/repo",
		`C:\repos\repo`,
		"file:///srv/repos/repo.git",
		"FILE:///srv/repos/repo.git",
		"http://github.com/org/repo.git",
		"ftp://example.com/repo.git",
		"HTTPS://github.com/org/repo.git",
		"ext::sh -c touch% /tmp/pwned",
		"fd::17",
		"https::github.com/org/repo",
		"-uhttps://github.com/org/repo",
		"--upload-pack=touch /tmp/pwned",
		" https://github.com/org/repo.git",
		"https://github.com/org/repo.git\n",
		"https://github.com/org/\x00repo",
		"ssh://-oProxyCommand=touch%20pwned/repo",
		"ssh://-oProxyCommand=x@host/repo",
		"-oProxyCommand=x:repo",
		"https:///org/repo",
		"host:",
		"/abs/path:with-colon",
		"dir/sub:path",
		"EXT::sh -c x",
		"x+y.z-1::addr",
		"https://ext::git-remote-probe.git",
	}
	for _, u := range refused {
		err := CheckHostCloneURL(u)
		require.Error(t, err, "%q must be refused", u)
		assert.True(t, errors.Is(err, ErrHostCloneURLRefused), u)
		if len(u) > 8 {
			assert.NotContains(t, err.Error(), u, "the refusal never echoes the URL")
		}
	}
}

// fakeGit puts a git on PATH that records its argv (one per line) and the
// GIT_ALLOW_PROTOCOL it ran with, then fails, and returns the record path.
func fakeGit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "argv")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + record + "'\n" +
		"printf 'ALLOW=%s\\n' \"$GIT_ALLOW_PROTOCOL\" >> '" + record + "'\n" +
		"exit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_ALLOW_PROTOCOL", "")
	return record
}

func readArgv(t *testing.T, record string) (argv []string, allow string) {
	t.Helper()
	data, err := os.ReadFile(record)
	require.NoError(t, err)
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if v, ok := strings.CutPrefix(l, "ALLOW="); ok {
			allow = v
			continue
		}
		argv = append(argv, l)
	}
	return argv, allow
}

// runGitClone ends the options with "--" before the URL and path, and a
// hardened clone carries HostCloneGitArgs ahead of "clone" and
// GIT_ALLOW_PROTOCOL=https:ssh:git.
func TestRunGitClone_ArgvEndsOptionsBeforeURL(t *testing.T) {
	const cloneURL = "https://git.example.com/org/repo.git"
	dest := filepath.Join(t.TempDir(), "ws")
	depth := 0

	for _, hardened := range []bool{false, true} {
		record := fakeGit(t)
		err := runGitClone(context.Background(), ProvisionInput{
			GitClone:        &api.GitCloneConfig{URL: cloneURL, Branch: "main", Depth: &depth},
			Resolved:        ResolvedWorkspace{HostPath: dest},
			HostGitHardened: hardened,
		})
		require.Error(t, err, "the fake git fails")
		argv, allow := readArgv(t, record)

		want := []string{}
		if hardened {
			want = append(want, HostCloneGitArgs()...)
		}
		want = append(want, "clone", "--branch", "main", "--", cloneURL, dest)
		assert.Equal(t, want, argv, "hardened=%v", hardened)
		if hardened {
			assert.Equal(t, HostCloneProtocols, allow)
		} else {
			assert.Empty(t, allow)
		}
	}
}

func TestHostCloneGitArgs_PinsTransports(t *testing.T) {
	args := HostCloneGitArgs()
	assert.Equal(t, hostGitArgs(), args[:len(hostGitArgs())], "starts with the host-side git settings")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"core.hooksPath=" + os.DevNull, "core.fsmonitor=false", "protocol.ext.allow=never",
		"protocol.allow=never", "protocol.https.allow=always", "protocol.ssh.allow=always", "protocol.git.allow=always",
	} {
		assert.Contains(t, joined, want)
	}
	assert.NotContains(t, joined, "protocol.http.allow", "plain http is not allowed")
	fileUser := strings.Index(joined, "protocol.file.allow=user")
	fileNever := strings.LastIndex(joined, "protocol.file.allow=never")
	assert.Greater(t, fileNever, fileUser, "the later file=never overrides file=user")
}

// Defence in depth below the URL check: a hardened clone of a local path or
// a file:// URL is refused by git itself and populates nothing.
func TestRunGitClone_HardenedRefusesLocalTransports(t *testing.T) {
	isolateGitConfig(t)
	bare := initBareGitRepo(t)
	depth := 0
	for _, src := range []string{bare, "file://" + bare} {
		dest := filepath.Join(t.TempDir(), "ws")
		err := runGitClone(context.Background(), ProvisionInput{
			GitClone:        &api.GitCloneConfig{URL: src, Depth: &depth},
			Resolved:        ResolvedWorkspace{HostPath: dest},
			HostGitHardened: true,
		})
		require.Error(t, err, "%s must be refused", src)
		assert.NoFileExists(t, filepath.Join(dest, "README.md"))
	}

	// The same local source clones without hardening (the existing callers).
	dest := filepath.Join(t.TempDir(), "ws")
	require.NoError(t, runGitClone(context.Background(), ProvisionInput{
		GitClone: &api.GitCloneConfig{URL: bare, Depth: &depth},
		Resolved: ResolvedWorkspace{HostPath: dest},
	}))
	assert.FileExists(t, filepath.Join(dest, "README.md"))
}
