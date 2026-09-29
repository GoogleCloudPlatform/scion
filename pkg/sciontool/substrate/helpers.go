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

package substrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var (
	errInvalidBootstrapPath    = errors.New("bootstrap file path must be a non-empty absolute path")
	errInvalidBootstrapContent = errors.New("bootstrap file content_b64 is not valid base64")
)

// Stable, machine-readable codes carried by bootstrapPathError (see its doc
// comment). These are wire contract: the runtime client
// (pkg/runtime/substrate_bootstrap.go) parses them out of the HTTP 422
// response body to decide how to report a rejected bootstrap file, so a
// value here must never change once shipped, only gain siblings.
const (
	// codeBootstrapPathSymlink names the errSymlinkComponent case: some
	// component of the file's path, at the time of the request, is a
	// symlink.
	codeBootstrapPathSymlink = "bootstrap_path_symlink"
	// codeBootstrapPathInvalid names every other path-shape rejection: an
	// empty or non-absolute Path, or a path component that exists but is
	// not a directory (errNonDirComponent).
	codeBootstrapPathInvalid = "bootstrap_path_invalid"
)

// bootstrapPathError is returned by writeBootstrapFile for a bootstrap file
// whose Path fails validation, and is what makes handleBootstrap answer
// HTTP 422 with a stable Code instead of the generic 500 every other write
// failure gets. Path is always the bootstrap file's own Path exactly as the
// caller sent it — never an internal ancestor mkdirAllTracked found the
// problem at, and never file content, which is secret-grade and must never
// reach a response or a log line (see deploy/substrate/README.md's "No
// symlink traversal in a target's path" note).
type bootstrapPathError struct {
	code   string
	path   string
	detail string // human-readable, content-free
}

func (e *bootstrapPathError) Error() string {
	return fmt.Sprintf("%s: bootstrap file %s rejected: %s", e.code, strconv.Quote(e.path), e.detail)
}

// errSymlinkComponent is returned by mkdirAllTracked when a path component
// that already exists on disk is a symlink. Bootstrap must never create or
// write through such a component — an image that ships, say,
// /home/scion/.config as a symlink to /etc could otherwise redirect a
// bootstrap write anywhere on the filesystem. The error names only the
// offending path (never any file content); writeBootstrapFile re-wraps it
// naming the bootstrap file's own Path before it reaches a log line.
type errSymlinkComponent struct {
	path string
}

func (e *errSymlinkComponent) Error() string {
	return fmt.Sprintf("path component %s is a symlink", e.path)
}

// errNonDirComponent is returned by mkdirAllTracked when a path component
// that already exists on disk is not a directory (e.g. a plain file sits
// where a bootstrap file's parent directory needs to be). Like
// errSymlinkComponent, this names only the offending path; writeBootstrapFile
// re-wraps it naming the bootstrap file's own Path.
type errNonDirComponent struct {
	path string
}

func (e *errNonDirComponent) Error() string {
	return fmt.Sprintf("path component %s is not a directory", e.path)
}

// mkdirAllTracked behaves like os.MkdirAll(dir, perm), but never follows a
// symlink in an existing path component, and also returns the list of
// directories it actually created (topmost first), so the caller can chown
// exactly the new directories rather than walking into pre-existing ones
// (e.g. /home) that must keep their current ownership.
//
// os.MkdirAll's own existence check is os.Stat, which follows symlinks: if
// an intermediate component already exists in the image as a symlink to a
// directory (e.g. /home/scion/.config -> /etc), Stat reports it as an
// ordinary existing directory and MkdirAll happily creates the remaining
// path components *through* the link.
//
// It is not enough to Lstat only the deepest ancestor that turns out to
// exist, walking upward until something is found: Lstat only declines to
// follow its own final argument, so a naive upward walk that stops at the
// first existing ancestor never Lstats anything *above* that point. If a
// symlinked component higher up the path already has the remaining subpath
// pre-created on its far side (e.g. .config -> /etc and /etc/sub already
// exists), the upward walk lands on that real directory and never notices
// the symlink at all. So this instead walks every existing component
// top-down, from the first path element to dir itself, Lstat-ing each one:
// the first symlink found anywhere is rejected, a non-dir is an error, and
// the first component that doesn't exist marks the start of the missing
// suffix, which is created top-down with plain os.Mkdir calls (never
// MkdirAll, which is Stat-based and would reopen the same hole one level
// down). Every directory os.Mkdir creates here is therefore known-real by
// construction — it did not exist a moment before this call created it.
//
// This check is safe against a symlink swapped in *before* bootstrap runs
// (a pre-built image), which is the only threat model bootstrap defends
// against: substrate-serve is the sole writer during bootstrap, the
// harness has not started yet, and the image is static up to this point.
// It is a plain existence check followed by a separate create, not an
// atomic operation, so it is not race-free against a concurrent writer that
// could swap a path component between the Lstat and the Mkdir (a TOCTOU
// window) — there is no such writer during bootstrap, so that window is not
// a guarantee this function makes for callers outside that model.
func mkdirAllTracked(dir string, perm os.FileMode) ([]string, error) {
	prefixes := pathPrefixes(filepath.Clean(dir))

	var created []string
	for i, p := range prefixes {
		info, err := os.Lstat(p)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, &errSymlinkComponent{path: p}
			}
			if !info.IsDir() {
				return nil, &errNonDirComponent{path: p}
			}
			continue
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		// p is the first missing component: it and everything below it (all
		// real, unchecked path — nothing above here was a symlink) must be
		// created, top-down so each Mkdir's parent already exists.
		for _, q := range prefixes[i:] {
			if err := os.Mkdir(q, perm); err != nil {
				return nil, err
			}
			created = append(created, q)
		}
		return created, nil
	}
	return created, nil
}

// pathPrefixes returns every path component of dir from the first one below
// the filesystem root down to dir itself, e.g. "/a/b/c" ->
// ["/a", "/a/b", "/a/b/c"]. dir must already be filepath.Clean-ed. The root
// itself ("/" or a Windows volume root) is deliberately not included: it
// always exists and cannot be a symlink, so Lstat-ing it would only cost a
// syscall without ever changing the result.
func pathPrefixes(dir string) []string {
	vol := filepath.VolumeName(dir)
	rest := strings.TrimPrefix(dir[len(vol):], string(filepath.Separator))
	if rest == "" {
		return nil
	}
	parts := strings.Split(rest, string(filepath.Separator))
	prefixes := make([]string, 0, len(parts))
	cur := vol + string(filepath.Separator)
	for _, part := range parts {
		cur = filepath.Join(cur, part)
		prefixes = append(prefixes, cur)
	}
	return prefixes
}

// redactErr returns err's message unless it looks like it might carry file
// contents (which could be secret material). Bootstrap file errors here are
// always structural (bad path, bad base64, mkdir/write failure) and never
// include content_b64 or decoded bytes, so this currently just documents
// that invariant rather than performing scrubbing. Kept as a named seam so
// future error paths added here must be checked against the same "no
// secrets in logs or errors" requirement (substrate-runtime.md §6).
func redactErr(err error) error { return err }
