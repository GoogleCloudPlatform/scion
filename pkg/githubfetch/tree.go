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

package githubfetch

import (
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	gitModeSymlink   = "120000"
	gitModeSubmodule = "160000"

	// maxTreeResponseBytes bounds how much of a git trees API response is read.
	// GitHub itself stops listing (and sets truncated) at about 7 MB.
	maxTreeResponseBytes int64 = 16 << 20
)

// File is one regular file listed in a GitHub git tree.
type File struct {
	Path    string
	Mode    os.FileMode // normalized 0o644 or 0o755
	Size    int64
	BlobSHA string
}

// Tree is the decoded listing of a folder's git tree at a commit.
type Tree struct {
	TreeSHA   string
	Files     []File
	Truncated bool
}

// decodeTree decodes a GitHub git trees API JSON response and enforces l's
// pre-download limits (truncated tree, file count, per-file size, total size,
// name length, submodule/symlink policies, and unsafe path rejection).
func decodeTree(body io.Reader, repo string, l Limits) (Tree, error) {
	l = l.withDefaults()
	var resp struct {
		SHA       string `json:"sha"`
		Truncated bool   `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
			Size int64  `json:"size"`
		} `json:"tree"`
	}
	if err := json.NewDecoder(io.LimitReader(body, maxTreeResponseBytes)).Decode(&resp); err != nil {
		return Tree{}, &Error{Kind: KindUpstream, Repo: repo, Hint: "invalid git tree response", err: err}
	}

	tree := Tree{
		TreeSHA:   resp.SHA,
		Truncated: resp.Truncated,
	}
	if resp.Truncated && l.MaxFiles > 0 {
		return tree, &Error{
			Kind: KindTooLarge,
			Repo: repo,
			Hint: "GitHub truncated the directory tree (too many entries)",
		}
	}

	var totalBytes int64
	for _, e := range resp.Tree {
		if e.Type == "commit" || e.Mode == gitModeSubmodule {
			if l.OnSubmodule == PolicyReject {
				return Tree{}, &Error{
					Kind: KindUnsupportedContent,
					Repo: repo,
					Hint: "directory contains a git submodule",
				}
			}
			continue
		}
		if e.Type != "blob" {
			continue
		}
		if e.Mode == gitModeSymlink {
			if l.OnSymlink == PolicyReject {
				return Tree{}, &Error{
					Kind: KindUnsupportedContent,
					Repo: repo,
					Hint: "directory contains a symlink",
				}
			}
			continue
		}
		if !validRelativePath(e.Path) {
			return Tree{}, &Error{
				Kind: KindUnsupportedContent,
				Repo: repo,
				Hint: "unsafe file path in git tree",
			}
		}
		if hasDotGitSegment(e.Path) {
			continue
		}
		if len(e.Path) > l.MaxNameBytes {
			return Tree{}, &Error{
				Kind: KindTooLarge,
				Repo: repo,
				Hint: "file path exceeds maximum name length",
			}
		}
		if e.Size < 0 || (l.MaxFileBytes > 0 && e.Size > l.MaxFileBytes) {
			return Tree{}, &Error{
				Kind: KindTooLarge,
				Repo: repo,
				Hint: "file exceeds maximum file size",
			}
		}
		totalBytes += e.Size
		if l.MaxTotal > 0 && totalBytes > l.MaxTotal {
			return Tree{}, &Error{
				Kind: KindTooLarge,
				Repo: repo,
				Hint: "directory exceeds maximum total size",
			}
		}
		tree.Files = append(tree.Files, File{
			Path:    e.Path,
			Mode:    normalizeGitMode(e.Mode),
			Size:    e.Size,
			BlobSHA: e.SHA,
		})
		if l.MaxFiles > 0 && len(tree.Files) > l.MaxFiles {
			return Tree{}, &Error{
				Kind: KindTooLarge,
				Repo: repo,
				Hint: "directory exceeds maximum file count",
			}
		}
	}
	return tree, nil
}

// normalizeGitMode converts a git tree mode string (e.g. "100755", "100644")
// into normalized 0o755 (any execute bit) or 0o644.
func normalizeGitMode(gitMode string) os.FileMode {
	if v, err := strconv.ParseUint(gitMode, 8, 32); err == nil {
		if v&0o111 != 0 {
			return 0o755
		}
	}
	return 0o644
}

// validRelativePath rejects empty paths, backslashes, NUL bytes, leading or
// trailing slashes, doubled slashes, and "." or ".." segments.
func validRelativePath(p string) bool {
	if p == "" || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func hasDotGitSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".git" {
			return true
		}
	}
	return false
}
