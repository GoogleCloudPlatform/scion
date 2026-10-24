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

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// Git tree entry modes the GitHub git trees API reports.
const (
	gitModeSymlink   = "120000"
	gitModeSubmodule = "160000"
)

// maxGitHubTreeBytes bounds how much of a git trees API response is read.
// GitHub itself stops listing (and sets truncated) at about 7 MB.
const maxGitHubTreeBytes = 16 * 1024 * 1024

// MaxGitHubSkillFiles is the most regular files a gh:// skill may have,
// counted after symlinks are skipped. The Contents API listing the
// resolvers used before was bounded by GitHub at 1000 entries per
// directory; the recursive tree listing is not, so this restores that
// bound for the whole skill. Both resolvers check it before downloading
// anything.
const MaxGitHubSkillFiles = 1000

// ErrGitHubSkillTooManyFiles is returned when a skill directory holds more
// than MaxGitHubSkillFiles regular files.
var ErrGitHubSkillTooManyFiles = fmt.Errorf("skill has more than %d files", MaxGitHubSkillFiles)

// ErrGitHubTreeTruncated is returned when GitHub truncated the tree of a
// skill directory. The tree is the skill's file list, so a truncated one
// would install an incomplete skill; the resolve fails instead.
var ErrGitHubTreeTruncated = errors.New("GitHub truncated the file tree of the skill directory (too many entries); split the skill into smaller directories")

// ErrGitHubTreeSubmodule is returned when a skill directory contains a git
// submodule. GitHub serves no content for a submodule path, so such a
// skill could never install.
var ErrGitHubTreeSubmodule = errors.New("skill directory contains a git submodule, which skills do not support")

// GitHubTreeFile is one regular file of a skill directory, as read from the
// GitHub git trees API.
type GitHubTreeFile struct {
	// Path is relative to the skill directory, with "/" separators.
	Path string
	// SHA is the file's git blob object ID.
	SHA string
	// Size is the blob size in bytes.
	Size int64
	// Mode is the normalised mode, "0755" (any execute bit set in the
	// repository) or "0644" (transfer.NormalizedModeString). Setuid,
	// setgid and sticky bits are never carried.
	Mode string
}

// GitHubSkillTreePath returns the API path (after the API base) of the
// recursive git tree of skillPath at commitSHA:
// /repos/{owner}/{repo}/git/trees/{commitSHA}:{skillPath}?recursive=1.
// The "<commit>:<path>" tree-ish names the skill directory's own tree, so
// the listing holds only that directory (not the whole repository) and
// its paths are relative to it. One call lists every file, at any depth,
// with its mode.
func GitHubSkillTreePath(owner, repo, skillPath, commitSHA string) string {
	return fmt.Sprintf("/repos/%s/%s/git/trees/%s:%s?recursive=1",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(commitSHA), escapePathSegments(skillPath))
}

// DecodeGitHubSkillTree reads a git trees API response for a skill
// directory and returns its regular files, sorted as GitHub lists them.
//
//   - A blob with mode 100755 (or any execute bit) becomes Mode "0755",
//     any other regular blob "0644".
//   - Symlinks (blobs with mode 120000) are skipped, as the Contents API
//     listing these resolvers used before skipped entries of type
//     "symlink".
//   - Subdirectories (type "tree") need no entry of their own: a
//     recursive listing names every file inside them.
//   - A submodule (type "commit", mode 160000) fails with
//     ErrGitHubTreeSubmodule. GitHub has no content for it; before, the
//     listing reported it as a "file" whose download then failed.
//   - A truncated listing fails with ErrGitHubTreeTruncated.
//   - More than MaxGitHubSkillFiles regular files fails with
//     ErrGitHubSkillTooManyFiles.
//   - A file path that could leave the skill directory (see
//     validateGitHubTreePath) fails the whole listing; it is never
//     cleaned into a different path.
func DecodeGitHubSkillTree(body io.Reader) ([]GitHubTreeFile, error) {
	var resp struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
			Size int64  `json:"size"`
		} `json:"tree"`
	}
	if err := json.NewDecoder(io.LimitReader(body, maxGitHubTreeBytes)).Decode(&resp); err != nil {
		return nil, fmt.Errorf("failed to decode GitHub tree response: %w", err)
	}
	if resp.Truncated {
		return nil, ErrGitHubTreeTruncated
	}
	var files []GitHubTreeFile
	for _, e := range resp.Tree {
		switch {
		case e.Type == "commit" || e.Mode == gitModeSubmodule:
			return nil, fmt.Errorf("%w: %s", ErrGitHubTreeSubmodule, e.Path)
		case e.Type != "blob":
			continue
		case e.Mode == gitModeSymlink:
			continue
		}
		if err := validateGitHubTreePath(e.Path); err != nil {
			return nil, err
		}
		files = append(files, GitHubTreeFile{
			Path: e.Path,
			SHA:  e.SHA,
			Size: e.Size,
			Mode: transfer.NormalizedModeString(e.Mode),
		})
		if len(files) > MaxGitHubSkillFiles {
			return nil, ErrGitHubSkillTooManyFiles
		}
	}
	return files, nil
}

// ErrGitHubTreeUnsafePath is returned when a tree entry's path is not a
// plain relative path inside the skill directory.
var ErrGitHubTreeUnsafePath = errors.New("unsafe file path in GitHub skill tree")

// validateGitHubTreePath rejects a tree path that is empty, absolute, or
// has an empty, "." or ".." segment (which also covers a leading,
// trailing or doubled "/") or a backslash. Git itself never records such
// paths; the check makes sure a malformed response cannot place a file
// outside the skill directory, and rejects rather than cleans so a
// response never resolves to a path it did not name.
func validateGitHubTreePath(p string) error {
	if p == "" || strings.Contains(p, "\\") {
		return fmt.Errorf("%w: %q", ErrGitHubTreeUnsafePath, p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: %q", ErrGitHubTreeUnsafePath, p)
		}
	}
	return nil
}

// IsGitHubSkillTreeError reports whether err is one of the skill tree
// failures DecodeGitHubSkillTree returns: a submodule, more than
// MaxGitHubSkillFiles files, a truncated listing or an unsafe path. These
// are deterministic properties of the skill at that commit, reported with
// SkillErrCodeUnsupportedContent on both resolver paths.
func IsGitHubSkillTreeError(err error) bool {
	return errors.Is(err, ErrGitHubTreeSubmodule) || errors.Is(err, ErrGitHubSkillTooManyFiles) ||
		errors.Is(err, ErrGitHubTreeTruncated) || errors.Is(err, ErrGitHubTreeUnsafePath)
}

// skillFilesHaveModes reports whether every file of skill records a mode.
// Resolvers that read GitHub trees set one on every file; an entry cached
// before they did has none, and is treated as absent so the ref is
// resolved again with its modes (ptone/scion#5781). Such an entry is
// still served if that re-resolve fails (see
// GitHubResolutionCache.resolveWithFetchAccept), as it would have been
// before.
func skillFilesHaveModes(files []ResolvedFile) bool {
	for _, f := range files {
		if f.Mode == "" {
			return false
		}
	}
	return true
}
