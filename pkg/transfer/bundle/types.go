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

// Package bundle implements the portable tar.gz part-split archive format used
// to transfer an agent's workspace and $HOME across Runtime Brokers during
// `scion reincarnate --broker`.
//
// API note relative to design §3.4: Write and Extract take an explicit
// scratchDir parameter (the broker's own temporary directory, never inside the
// source or staging root; empty falls back to os.TempDir) so part buffering
// and hash-before-gunzip staging stay outside the agent tree and callers can
// bound and verify temp-file cleanup.
package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// ManifestVersion is the only bundle manifest version supported by this package.
const ManifestVersion = "bundle/1"

// Canonical root names inside a bundle Manifest.
const (
	RootWorkspace = "workspace"
	RootHome      = "home"
)

// Default limits from design §3.7.
const (
	DefaultPartSize            int64 = 64 << 20 // 64 MiB compressed per part
	MinManifestPartSize        int64 = 1 << 20  // 1 MiB floor when rm.PartSize != lim.PartSize
	DefaultWorkspaceMaxBytes   int64 = 4 << 30  // 4 GiB uncompressed
	DefaultWorkspaceMaxEntries int64 = 250_000
	DefaultHomeMaxBytes        int64 = 1 << 30 // 1 GiB uncompressed
	DefaultHomeMaxEntries      int64 = 100_000
	DefaultMaxDepth            int   = 64
	DefaultMaxPathLen          int   = 4096
)

// Sentinel errors returned by manifest validation and extraction.
var (
	ErrUnsupportedVersion   = errors.New("unsupported bundle manifest version")
	ErrInvalidManifest      = errors.New("invalid bundle manifest")
	ErrHashMismatch         = errors.New("bundle part hash mismatch")
	ErrPartSizeMismatch     = errors.New("bundle part size mismatch")
	ErrDuplicateEntry       = errors.New("duplicate bundle archive entry")
	ErrSymlinkParent        = errors.New("bundle entry parent is not a directory")
	ErrEscapingSymlink      = errors.New("bundle symlink escapes root")
	ErrUnsupportedEntryType = errors.New("unsupported bundle archive entry type")
	ErrTruncatedStream      = errors.New("truncated bundle stream")
)

// Manifest is the Hub-persisted, target-verified description of a bundle.
type Manifest struct {
	Version         string                  `json:"version"`
	ReincarnationID string                  `json:"reincarnationId"`
	Roots           map[string]RootManifest `json:"roots"`
}

// RootManifest describes the ordered compressed parts and statistics for one
// bundled root ("workspace" or "home").
type RootManifest struct {
	Parts    []Part     `json:"parts"`
	PartSize int64      `json:"partSize"`
	Entries  int64      `json:"entries"`
	Bytes    int64      `json:"bytes"`
	Skipped  SkipCounts `json:"skipped"`
}

// Part describes one compressed slice of a root's tar.gz stream.
type Part struct {
	Index int    `json:"index"`
	Size  int64  `json:"size"`
	Hash  string `json:"hash"` // sha256:<hex> of compressed bytes
}

// SkipCounts records entries omitted while building a root archive.
type SkipCounts struct {
	Sockets int64 `json:"sockets,omitempty"`
	FIFOs   int64 `json:"fifos,omitempty"`
	Devices int64 `json:"devices,omitempty"`
	// EscapingSymlinks counts skipped symlinks whose targets are absolute,
	// escape the root (including multi-hop chains), or exceed maxSymlinkHops
	// (such as symlink loops).
	EscapingSymlinks int64 `json:"escapingSymlinks,omitempty"`
	Excluded         int64 `json:"excluded,omitempty"`
}

// Limits bounds the size, entry count, path depth, path length, and part size
// of a bundle write or extraction.
type Limits struct {
	MaxBytes   int64 `json:"maxBytes"`
	MaxEntries int64 `json:"maxEntries"`
	MaxDepth   int   `json:"maxDepth"`
	MaxPathLen int   `json:"maxPathLen"`
	PartSize   int64 `json:"partSize"`
}

// DefaultWorkspaceLimits returns the default bounds for a workspace root.
func DefaultWorkspaceLimits() Limits {
	return Limits{
		MaxBytes:   DefaultWorkspaceMaxBytes,
		MaxEntries: DefaultWorkspaceMaxEntries,
		MaxDepth:   DefaultMaxDepth,
		MaxPathLen: DefaultMaxPathLen,
		PartSize:   DefaultPartSize,
	}
}

// DefaultHomeLimits returns the default bounds for a home root.
func DefaultHomeLimits() Limits {
	return Limits{
		MaxBytes:   DefaultHomeMaxBytes,
		MaxEntries: DefaultHomeMaxEntries,
		MaxDepth:   DefaultMaxDepth,
		MaxPathLen: DefaultMaxPathLen,
		PartSize:   DefaultPartSize,
	}
}

// Validate checks that all limit fields are positive.
func (l Limits) Validate() error {
	if l.MaxBytes <= 0 {
		return fmt.Errorf("invalid limits: maxBytes must be > 0 (got %d)", l.MaxBytes)
	}
	if l.MaxEntries <= 0 {
		return fmt.Errorf("invalid limits: maxEntries must be > 0 (got %d)", l.MaxEntries)
	}
	if l.MaxDepth <= 0 {
		return fmt.Errorf("invalid limits: maxDepth must be > 0 (got %d)", l.MaxDepth)
	}
	if l.MaxPathLen <= 0 {
		return fmt.Errorf("invalid limits: maxPathLen must be > 0 (got %d)", l.MaxPathLen)
	}
	if l.PartSize <= 0 {
		return fmt.Errorf("invalid limits: partSize must be > 0 (got %d)", l.PartSize)
	}
	return nil
}

// MaxPartsFor returns the maximum number of parts allowed for the given
// maxBytes and partSize, per design §3.7: ceil(maxBytes / partSize) + 8.
func MaxPartsFor(maxBytes, partSize int64) int {
	if partSize <= 0 || maxBytes <= 0 {
		return 0
	}
	return int((maxBytes+partSize-1)/partSize) + 8
}

// MaxParts returns the maximum number of parts allowed for these limits,
// per design §3.7: ceil(MaxBytes / PartSize) + 8.
func (l Limits) MaxParts() int {
	return MaxPartsFor(l.MaxBytes, l.PartSize)
}

// ErrLimitExceeded is returned when writing, estimating, or extracting a bundle
// crosses a configured bound.
type ErrLimitExceeded struct {
	Field string
	Got   int64
	Max   int64
}

func (e *ErrLimitExceeded) Error() string {
	return fmt.Sprintf("bundle exceeds limit: %s %d > %d", e.Field, e.Got, e.Max)
}

// ErrUnportableHome is returned by Write and Estimate when an exclusion entry
// has an ancestor symlink that cannot be resolved inside the root (for example,
// an absolute symlink pointing outside AgentHomeAbs, a relative symlink
// escaping the root, or a symlink loop). Failing closed prevents excluded
// secrets from being bundled under an unresolved physical path.
type ErrUnportableHome struct {
	Path   string
	Reason string
}

func (e *ErrUnportableHome) Error() string {
	return fmt.Sprintf("unportable home: %s (%s)", e.Path, e.Reason)
}

// ErrUnportablePath is returned by Write and Estimate when a source entry has
// a filename that fails transfer.ValidateRelPath (for example, a filename
// containing a backslash on Unix).
type ErrUnportablePath struct {
	Path string
	Err  error
}

func (e *ErrUnportablePath) Error() string {
	return fmt.Sprintf("unportable path %q: %v", e.Path, e.Err)
}

func (e *ErrUnportablePath) Unwrap() error {
	return e.Err
}

// PartSink receives each completed compressed part from Write.
// part is a seekable temporary file positioned at offset 0. The sink must not
// retain part after Put returns; Write closes and deletes it immediately.
type PartSink interface {
	Put(ctx context.Context, index int, part io.ReadSeeker, size int64, hash string) error
}

// PartSource supplies the compressed bytes of a part to Extract.
type PartSource interface {
	Get(ctx context.Context, p Part) (io.ReadCloser, error)
}

// EstimateResult holds the stat-only size and entry counts for a root.
type EstimateResult struct {
	Entries int64      `json:"entries"`
	Bytes   int64      `json:"bytes"`
	Skipped SkipCounts `json:"skipped"`
}

// Validate checks that m has the supported ManifestVersion and well-formed
// root manifests.
func (m Manifest) Validate() error {
	if m.Version != ManifestVersion {
		return fmt.Errorf("%w: %q (want %q)", ErrUnsupportedVersion, m.Version, ManifestVersion)
	}
	if len(m.Roots) == 0 {
		return fmt.Errorf("%w: manifest has no roots", ErrInvalidManifest)
	}
	for name, rm := range m.Roots {
		if name != RootWorkspace && name != RootHome {
			return fmt.Errorf("%w: unknown root %q", ErrInvalidManifest, name)
		}
		if err := rm.Validate(); err != nil {
			return fmt.Errorf("root %q: %w", name, err)
		}
	}
	return nil
}

// Validate checks that a RootManifest has a positive PartSize and contiguous
// zero-based parts with positive sizes and valid sha256 content hashes.
func (rm RootManifest) Validate() error {
	if len(rm.Parts) == 0 {
		return fmt.Errorf("%w: root has no parts", ErrInvalidManifest)
	}
	if rm.PartSize <= 0 {
		return fmt.Errorf("%w: non-positive partSize %d", ErrInvalidManifest, rm.PartSize)
	}
	if rm.Entries < 0 || rm.Bytes < 0 {
		return fmt.Errorf("%w: negative counts", ErrInvalidManifest)
	}
	for i, p := range rm.Parts {
		if p.Index != i {
			return fmt.Errorf("%w: part %d has index %d", ErrInvalidManifest, i, p.Index)
		}
		if p.Size <= 0 {
			return fmt.Errorf("%w: part %d has non-positive size %d", ErrInvalidManifest, i, p.Size)
		}
		if !transfer.IsContentHash(p.Hash) {
			return fmt.Errorf("%w: part %d has invalid hash %q", ErrInvalidManifest, i, p.Hash)
		}
	}
	return nil
}

// ParseManifest unmarshals a JSON-encoded Manifest and validates it, refusing
// unknown manifest versions.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// CanonicalManifest encodes m as canonical JSON (sorted map keys, no trailing
// whitespace) and returns the bytes alongside their "sha256:<hex>" digest.
func CanonicalManifest(m Manifest) ([]byte, string, error) {
	if err := m.Validate(); err != nil {
		return nil, "", err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, "", err
	}
	return b, transfer.HashBytes(b), nil
}

const maxSymlinkHops = 40

// validateSymlinkTarget reports whether target is a lexically safe relative
// symlink target for an entry at slash-separated relPath inside a bundle root.
// Absolute targets, targets with NUL or backslashes, and targets that lexically
// resolve outside the root return ErrEscapingSymlink.
func validateSymlinkTarget(relPath, target string) error {
	if target == "" || strings.ContainsRune(target, 0) || strings.Contains(target, `\`) {
		return ErrEscapingSymlink
	}
	if path.IsAbs(target) || filepath.IsAbs(target) {
		return ErrEscapingSymlink
	}
	entryDir := path.Dir(relPath)
	resolved := path.Clean(path.Join(entryDir, target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") || path.IsAbs(resolved) {
		return ErrEscapingSymlink
	}
	return nil
}

// validateSymlinkInRoot checks both lexical containment and physical symlink
// chain containment inside root: walking target component-by-component from
// relPath's parent directory, any intermediate or final symlink is expanded
// (up to maxSymlinkHops) and must never step above root or point to an
// absolute/invalid target.
func validateSymlinkInRoot(root *os.Root, relPath, target string) error {
	if err := validateSymlinkTarget(relPath, target); err != nil {
		return err
	}
	_, err := resolvePathInRoot(root, path.Dir(relPath), target)
	return err
}

// resolvePathInRoot resolves target relative to baseDir inside root component
// by component, following any in-root symlinks up to maxSymlinkHops. It
// returns the resolved slash-separated path relative to root ("" for the root
// directory itself), or ErrEscapingSymlink if any link escapes root, is
// absolute/invalid, or exceeds maxSymlinkHops.
func resolvePathInRoot(root *os.Root, baseDir, target string) (string, error) {
	var cur []string
	if baseDir != "." && baseDir != "" {
		cur = strings.Split(baseDir, "/")
	}
	queue := strings.Split(target, "/")
	hops := 0

	for len(queue) > 0 {
		comp := queue[0]
		queue = queue[1:]

		switch comp {
		case "", ".":
			continue
		case "..":
			if len(cur) == 0 {
				return "", ErrEscapingSymlink
			}
			cur = cur[:len(cur)-1]
			continue
		}

		cur = append(cur, comp)
		cand := strings.Join(cur, "/")
		fi, err := root.Lstat(filepath.FromSlash(cand))
		if err != nil {
			// A missing target (dangling internal symlink) or non-directory
			// intermediate component cannot redirect via a symlink; continue
			// checking any remaining ".." lexically.
			continue
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			continue
		}

		hops++
		if hops > maxSymlinkHops {
			return "", ErrEscapingSymlink
		}
		linkTarget, err := root.Readlink(filepath.FromSlash(cand))
		if err != nil {
			return "", ErrEscapingSymlink
		}
		if linkTarget == "" || strings.ContainsRune(linkTarget, 0) || strings.Contains(linkTarget, `\`) ||
			path.IsAbs(linkTarget) || filepath.IsAbs(linkTarget) {
			return "", ErrEscapingSymlink
		}
		// Resolve linkTarget relative to cand's parent directory.
		cur = cur[:len(cur)-1]
		next := strings.Split(linkTarget, "/")
		queue = append(next, queue...)
	}
	return strings.Join(cur, "/"), nil
}

// pathDepth returns the 1-based depth of a validated slash-separated relative path.
func pathDepth(relPath string) int {
	return strings.Count(relPath, "/") + 1
}

// maxPathBytesBudget returns the total path+link-target byte budget shared by
// Write, Estimate, and Extract so precheck, export, and import agree.
func maxPathBytesBudget(lim Limits) int64 {
	return max(lim.MaxEntries*256, int64(lim.MaxPathLen))
}

func entryPathBytesLen(relPath, linkTarget string) int64 {
	return int64(len(relPath)) + int64(len(linkTarget))
}
