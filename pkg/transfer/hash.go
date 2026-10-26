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

// contentHashPattern matches a content hash produced by this package:
// HashPrefix followed by 64 lowercase hex characters.
var contentHashPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// IsContentHash reports whether s is a content hash in the "sha256:<hex64>"
// form produced by HashBytes, ComputeContentHashV1 and ComputeContentHashV2.
// Content-addressed caches (for example the broker's template cache) use this value as a
// directory name, so callers use it to tell such a directory name apart from
// a human-chosen name.
func IsContentHash(s string) bool {
	return contentHashPattern.MatchString(s)
}

// gitBlobHashPattern matches a bare git object ID: 40 lowercase hex characters.
var gitBlobHashPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// IsGitBlobHash reports whether s is formatted as a git blob object ID
// (40 lowercase hex characters, no algorithm prefix).
//
// It is used to tell apart the two hash formats that flow through skill
// resolution: "sha256:<hex64>" digests produced by this package, and bare git
// blob object IDs supplied verbatim by GitHub's git trees API.
func IsGitBlobHash(s string) bool {
	return gitBlobHashPattern.MatchString(s)
}

// GitBlobHashBytes computes the git blob object ID of data, i.e.
// SHA-1("blob <len>\x00" + data), returned as bare lowercase hex.
//
// This is the value GitHub's git trees and Contents APIs report in the
// "sha" field of a file entry. Computing it locally lets a caller verify content fetched from
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
// Returns the hash in format "sha256:<hex>". The file is opened
// non-blocking on Unix, so a FIFO at path does not block the open.
func HashFile(path string) (string, error) {
	f, err := os.OpenFile(path, readOnlyFlags, 0)
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

// ComputeContentHashV1 is the original aggregate content hash: SHA-256 over
// the per-file hashes concatenated in path order. Paths and file modes are
// not part of the preimage, so a rename-only or chmod-only change keeps the
// same value (ptone/scion#5658).
//
// It remains the recorded skill bundle hash (the contentHash of every skill
// response): published skill versions are immutable and pinned by this
// value (registry pins, content-addressed version lookup), and brokers
// before ptone/scion#5935 verify an installed bundle by recomputing it.
// Skill hash v2 (ComputeContentHashV2 over the same files) is derived
// alongside it while the hub.skill_content_hash_v2 experiment is on and is
// accepted as well as v1 for pins and lookups (ptone/scion#5935).
//
// For templates and harness-configs the hub records it while the
// hub.content_hash_v2 experiment is off, and ComputeContentHashV2 while it
// is on. The hub never uses it to accept a client's expected hash
// against a v2 row: the commit compare-and-swap compares the stored value
// strictly (accepting a v1 value that matches the v1 hash of the stored
// files would accept a stale commit over a rename-only or chmod-only
// change). The CLI uses it only to compare with a hub response that has no
// manifest.
// Returns "" for an empty file list.
func ComputeContentHashV1(files []FileInfo) string {
	if len(files) == 0 {
		return ""
	}

	sorted := sortedByPath(files)
	hasher := sha256.New()
	for _, file := range sorted {
		hasher.Write([]byte(file.Hash))
	}

	return HashPrefix + hex.EncodeToString(hasher.Sum(nil))
}

// ComputeContentHashV2 is the content hash of templates and harness-configs
// (ptone/scion#5658), and skill content hash v2 (ptone/scion#5935): SHA-256 over one line per file, in path order,
//
//	<path> NUL <normalised mode> NUL <file hash> LF
//
// where the mode is NormalizedModeString ("0644" or "0755"; an empty mode
// is "0644"). A rename, a chmod that flips the
// executable bit, and a content change each change the value. It has the
// same "sha256:<64 hex>" form as ComputeContentHashV1 (IsContentHash).
// Returns "" for an empty file list.
func ComputeContentHashV2(files []FileInfo) string {
	if len(files) == 0 {
		return ""
	}

	sorted := sortedByPath(files)
	hasher := sha256.New()
	for _, file := range sorted {
		hasher.Write([]byte(file.Path))
		hasher.Write([]byte{0})
		hasher.Write([]byte(NormalizedModeString(file.Mode)))
		hasher.Write([]byte{0})
		hasher.Write([]byte(file.Hash))
		hasher.Write([]byte{'\n'})
	}

	return HashPrefix + hex.EncodeToString(hasher.Sum(nil))
}

// sortedByPath returns a copy of files sorted by path.
func sortedByPath(files []FileInfo) []FileInfo {
	sorted := make([]FileInfo, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Path < sorted[j].Path
	})
	return sorted
}
