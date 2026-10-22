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

package runtimebroker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
)

// Workspace sync never carries a repository's configuration (see
// transfer.ClassifyWorkspaceSyncPath): a workspace downloaded onto a broker
// for the first time gets the repository's history but no .git/config. The
// broker writes a minimal one itself, from what it knows, so the checkout
// works and can push.

// minimalGitConfigCore is the [core] section of a minimal repository
// configuration. It holds no key that names a program, a path or a
// command (no hooksPath, fsmonitor, sshCommand, editor, pager, alias or
// filter).
const minimalGitConfigCore = "[core]\n" +
	"\trepositoryformatversion = 0\n" +
	"\tfilemode = true\n" +
	"\tbare = false\n" +
	"\tlogallrefupdates = true\n"

// scpCloneURL matches the scp-style ssh remote the hub keeps as a clone URL
// ("git@host:org/repo"; see util.NormalizeCloneURL).
var scpCloneURL = regexp.MustCompile(`^git@[A-Za-z0-9.\-\[\]:]+:[A-Za-z0-9._~/\-]+$`)

// errUnsafeCloneURL marks a clone URL that is not written into a
// repository configuration.
var errUnsafeCloneURL = errors.New("clone URL is not a network git remote")

// validateOriginURL checks that cloneURL can be written as remote.origin.url:
// an http(s)://, ssh:// or git:// URL, or an scp-style git@host:path remote.
// Local paths, file:// URLs and transport-helper forms such as ext:: or
// file:: are refused (a fetch from them can run a program or read the
// broker's filesystem), and so are characters that could end the value or
// start a new configuration line.
func validateOriginURL(cloneURL string) error {
	if cloneURL == "" || strings.HasPrefix(cloneURL, "-") || strings.Contains(cloneURL, "::") {
		return errUnsafeCloneURL
	}
	for _, r := range cloneURL {
		if r < 0x21 || r > 0x7e || r == '"' || r == '\\' || r == ';' || r == '#' {
			return errUnsafeCloneURL
		}
	}
	for _, scheme := range []string{"https://", "http://", "ssh://", "git://"} {
		if strings.HasPrefix(strings.ToLower(cloneURL), scheme) && len(cloneURL) > len(scheme) {
			return nil
		}
	}
	if scpCloneURL.MatchString(cloneURL) {
		return nil
	}
	return errUnsafeCloneURL
}

// minimalGitConfig returns the minimal repository configuration, with an
// origin remote when originURL is not empty (already validated).
func minimalGitConfig(originURL string) string {
	cfg := minimalGitConfigCore
	if originURL != "" {
		cfg += "[remote \"origin\"]\n" +
			"\turl = " + originURL + "\n" +
			"\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	}
	return cfg
}

// ensureMinimalGitConfig writes a minimal .git/config into the workspace at
// workspaceDir when its root .git is a real directory without a config. The
// origin remote is set from cloneURL when it passes validateOriginURL;
// otherwise the configuration has no remote and the returned error (which
// names no path) says why. An existing config, a .git file (a worktree's
// gitdir pointer), a symlink or a missing .git is left alone. It reports
// whether it wrote a configuration.
func ensureMinimalGitConfig(workspaceDir, cloneURL string) (bool, error) {
	root, err := os.OpenRoot(workspaceDir)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()

	info, err := root.Lstat(".git")
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	if _, err := root.Lstat(".git/config"); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}

	origin := ""
	urlErr := validateOriginURL(cloneURL)
	if cloneURL != "" && urlErr == nil {
		origin = cloneURL
	}

	gitDir, err := root.OpenRoot(".git")
	if err != nil {
		return false, err
	}
	defer func() { _ = gitDir.Close() }()
	opened, err := gitDir.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return false, errors.New("workspace .git changed while writing its configuration")
	}
	f, err := gitDir.OpenFile("config", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return false, err
	}
	_, werr := f.WriteString(minimalGitConfig(origin))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return false, werr
	}
	if cloneURL != "" && urlErr != nil {
		return true, fmt.Errorf("origin remote not set: %w", urlErr)
	}
	return true, nil
}
