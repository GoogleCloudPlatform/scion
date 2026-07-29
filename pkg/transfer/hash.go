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

package transfer

import (
	"crypto/sha1" //nolint:gosec // Git object IDs are SHA-1 by definition; see GitBlobHashFile.
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
)

// HashPrefix is the prefix for SHA-256 hashes.
const HashPrefix = "sha256:"

// gitBlobHashPattern matches a bare git object ID: 40 lowercase hex characters.
var gitBlobHashPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// IsGitBlobHash reports whether s is formatted as a git blob object ID
// (40 lowercase hex characters, no algorithm prefix).
//
// It is used to tell apart the two hash formats that flow through skill
// resolution: "sha256:<hex64>" digests produced by this package, and bare git
// blob object IDs supplied verbatim by GitHub's Contents API.
func IsGitBlobHash(s string) bool {
	return gitBlobHashPattern.MatchString(s)
}

// GitBlobHashBytes computes the git blob object ID of data, i.e.
// SHA-1("blob <len>\x00" + data), returned as bare lowercase hex.
//
// This is the value GitHub's Contents API reports in the "sha" field of a
// file entry. Computing it locally lets a caller verify content fetched from
// GitHub against metadata GitHub already published, without the metadata
// producer having to download the content itself.
//
// SHA-1 is used because git's object model mandates it. This is not a
// general-purpose integrity primitive — prefer HashBytes/HashFile for that.
func GitBlobHashBytes(data []byte) string {
	h := sha1.New() //nolint:gosec // Git object IDs are SHA-1 by definition.
	// hash.Hash writes never return an error, but errcheck cannot know that.
	_, _ = fmt.Fprintf(h, "blob %d\x00", len(data))
	_, _ = h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// GitBlobHashFile computes the git blob object ID of a file's contents.
// See GitBlobHashBytes for the format and the rationale for SHA-1.
func GitBlobHashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return "", err
	}

	h := sha1.New() //nolint:gosec // Git object IDs are SHA-1 by definition.
	// hash.Hash writes never return an error, but errcheck cannot know that.
	_, _ = fmt.Fprintf(h, "blob %d\x00", info.Size())
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashFile computes the SHA-256 hash of a file.
// Returns the hash in format "sha256:<hex>".
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", err
	}

	return HashPrefix + hex.EncodeToString(hasher.Sum(nil)), nil
}

// HashBytes computes the SHA-256 hash of a byte slice.
// Returns the hash in format "sha256:<hex>".
func HashBytes(data []byte) string {
	hasher := sha256.New()
	hasher.Write(data)
	return HashPrefix + hex.EncodeToString(hasher.Sum(nil))
}

// ComputeContentHash computes the overall content hash from a list of file hashes.
// Files are sorted by path for deterministic ordering before hash computation.
// Returns the hash in format "sha256:<hex>".
func ComputeContentHash(files []FileInfo) string {
	if len(files) == 0 {
		return ""
	}

	// Sort files by path for deterministic ordering
	sorted := make([]FileInfo, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Path < sorted[j].Path
	})

	// Concatenate hashes and compute final hash
	hasher := sha256.New()
	for _, file := range sorted {
		hasher.Write([]byte(file.Hash))
	}

	return HashPrefix + hex.EncodeToString(hasher.Sum(nil))
}
