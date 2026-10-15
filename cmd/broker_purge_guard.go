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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
)

// brokerStateEntryKind is the kind of a broker-local state entry that
// 'runtime-broker deregister --purge-local' handles (brokerLocalStatePaths).
type brokerStateEntryKind int

const (
	// stateEntryDaemonLog is the standalone broker daemon log,
	// daemon.GetLogPath(Root) with Root the global directory.
	stateEntryDaemonLog brokerStateEntryKind = iota
	// stateEntryStateDir is the broker's state directory,
	// <Root>/<BrokerID> with Root the state root.
	stateEntryStateDir
	// stateEntryTemplateCache is the broker template cache,
	// <Root>/cache/templates with Root the scion home.
	stateEntryTemplateCache
)

func (k brokerStateEntryKind) String() string {
	switch k {
	case stateEntryDaemonLog:
		return "broker daemon log"
	case stateEntryStateDir:
		return "broker state directory"
	case stateEntryTemplateCache:
		return "broker template cache"
	}
	return "unknown entry"
}

// brokerLocalStateEntry is one broker-local state entry: its kind, its
// path, the root it is expected under, and (state directory only) the
// broker ID that names it.
type brokerLocalStateEntry struct {
	Kind     brokerStateEntryKind
	Path     string
	Root     string
	BrokerID string
}

// entryPaths returns the paths of entries, in order.
func entryPaths(entries []brokerLocalStateEntry) []string {
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	return paths
}

// existingEntries returns the entries whose path exists.
func existingEntries(entries []brokerLocalStateEntry) []brokerLocalStateEntry {
	var out []brokerLocalStateEntry
	for _, e := range entries {
		if _, err := os.Lstat(e.Path); err == nil {
			out = append(out, e)
		}
	}
	return out
}

// checkPurgeEntry reports why e is not at the location its kind expects,
// or nil when it is. Every entry must be an absolute, clean path that is
// neither, nor resolves to, the filesystem root or the user's home
// directory (os.UserHomeDir, the home the scion home is derived from).
// Then, by kind:
//   - daemon log: exactly daemon.GetLogPath(Root), and not a directory;
//   - state directory: BrokerID is a single path element, Path is exactly
//     <Root>/<BrokerID>, and Root is absolute and neither is, nor resolves
//     to, the filesystem root or the home directory;
//   - template cache: Path is exactly <Root>/cache/templates, with Root
//     checked as for the state directory.
func checkPurgeEntry(e brokerLocalStateEntry) error {
	if err := checkPurgeLocation(e.Path); err != nil {
		return err
	}
	switch e.Kind {
	case stateEntryDaemonLog:
		if e.Path != daemon.GetLogPath(e.Root) {
			return fmt.Errorf("not the broker daemon log %s", daemon.GetLogPath(e.Root))
		}
		if fi, err := os.Lstat(e.Path); err == nil && fi.IsDir() {
			return errors.New("the broker daemon log path is a directory")
		}
		return nil
	case stateEntryStateDir:
		if err := brokercredentials.ValidateBrokerIDPathElement(e.BrokerID); err != nil {
			return err
		}
		if e.Path != filepath.Join(e.Root, e.BrokerID) {
			return fmt.Errorf("not <state root>/<broker-id> under %s", e.Root)
		}
		return checkPurgeRoot(e.Root, "state root")
	case stateEntryTemplateCache:
		if e.Path != filepath.Join(e.Root, "cache", "templates") {
			return fmt.Errorf("not <scion home>/cache/templates under %s", e.Root)
		}
		return checkPurgeRoot(e.Root, "scion home")
	}
	return fmt.Errorf("unknown entry kind %d", e.Kind)
}

// checkPurgeRoot refuses a root that is not absolute or that is, or
// resolves to, the filesystem root or the home directory.
func checkPurgeRoot(root, what string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("the %s %q is not absolute", what, root)
	}
	if err := checkPurgeLocation(root); err != nil {
		return fmt.Errorf("the %s %s", what, err)
	}
	return nil
}

// checkPurgeLocation refuses a path that is not absolute and clean, or
// that is, or resolves to, the filesystem root or the home directory.
func checkPurgeLocation(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("%q is not an absolute, clean path", path)
	}
	resolved, err := resolveExistingPrefix(path)
	if err != nil {
		return fmt.Errorf("cannot resolve %s: %v", path, err)
	}
	if path == string(filepath.Separator) || resolved == string(filepath.Separator) {
		return errors.New("is the filesystem root")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		home = filepath.Clean(home)
		resolvedHome, _ := resolveExistingPrefix(home)
		if path == home || resolved == home || (resolvedHome != "" && resolved == resolvedHome) {
			return errors.New("is the home directory")
		}
	}
	return nil
}

// resolveExistingPrefix returns path with the symlinks of its deepest
// existing ancestor resolved and the missing tail rejoined.
func resolveExistingPrefix(path string) (string, error) {
	dir := filepath.Clean(path)
	var tail []string
	for {
		if _, err := os.Lstat(dir); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		tail = append([]string{filepath.Base(dir)}, tail...)
		dir = parent
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{resolved}, tail...)...), nil
}

// purgeBrokerLocalState removes the entries that exist, after checking
// every entry against the location its kind expects (checkPurgeEntry).
// When any entry fails, nothing is removed: each refused entry is printed
// with its reason and an error is returned (a non-zero exit).
func purgeBrokerLocalState(entries []brokerLocalStateEntry, out io.Writer) error {
	var refused []string
	for _, e := range entries {
		if err := checkPurgeEntry(e); err != nil {
			refused = append(refused, fmt.Sprintf("%s (%s: %v)", e.Path, e.Kind, err))
		}
	}
	if len(refused) > 0 {
		_, _ = fmt.Fprintln(out, "Not an expected broker state location, not removed:")
		for _, r := range refused {
			_, _ = fmt.Fprintf(out, "  %s\n", r)
		}
		return fmt.Errorf("--purge-local skipped: %d entr%s not at an expected broker state location; nothing was removed", len(refused), pluralY(len(refused)))
	}
	var errs []error
	for _, e := range existingEntries(entries) {
		if err := os.RemoveAll(e.Path); err != nil {
			errs = append(errs, fmt.Errorf("failed to remove %s: %w", e.Path, err))
			continue
		}
		_, _ = fmt.Fprintf(out, "Removed %s\n", e.Path)
	}
	return errors.Join(errs...)
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
