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
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// enforcedHooksHomePrefix is $HOME/.scion/hooks — "the scion home as
// bootstrap knows it": util.GetHomeDir("scion"), the same "/home/<user>"
// convention every other substrate write path uses (buildBootstrapEnv,
// homeBootstrapFiles' containerHome, supervisor.Supervisor.Run's own HOME
// override), never a value taken from the bootstrap request itself. A fixed
// package var, not a const, purely so a test can point it at a throwaway
// prefix without touching a real "/home/scion".
var enforcedHooksHomePrefix = filepath.Join(util.GetHomeDir("scion"), ".scion", "hooks")

// enforcedHooksDir is redirectEnforcedHooksPath's and clearEnforcedHooksDir's
// own copy of hooks.EnforcedHooksDir, as a package var rather than a direct
// reference to that constant, purely so a test can point the redirect target
// at a throwaway directory instead of the real, root-owned "/run/scion/hooks"
// (which a test has no business writing to, and — running unprivileged —
// usually can't). Production code never reassigns it.
var enforcedHooksDir = hooks.EnforcedHooksDir

// redirectEnforcedHooksPath maps a cleaned bootstrap file path that falls
// under enforcedHooksHomePrefix to hooks.EnforcedHooksDir/<rest>, leaving
// every other path untouched.
//
// This is what keeps broker-delivered lifecycle hook content (the
// container-script harness's trusted pre-start wrapper, and any project/hub
// pre-start hook) root-owned in privilege-drop-enforced mode, instead of
// falling under the same "chown everything bootstrap writes to the
// workload" rule the rest of the composed home gets — see writeBootstrapFile
// below for that chown step, and pkg/sciontool/hooks.DecideExecAsRoot for
// why a workload-owned hook script must never run as root.
//
// path must already be filepath.Clean-ed by the caller (writeBootstrapFile
// cleans f.Path before this is ever called): matching against the cleaned
// form is what makes a ".." trick or a doubled separator unable to either
// dodge or spoof the prefix match — there is no separate traversal check
// here because Clean has already resolved any ".." lexically before this
// string comparison ever runs.
func redirectEnforcedHooksPath(path string) (string, bool) {
	if path == enforcedHooksHomePrefix {
		// The bare directory itself, with nothing under it — nothing
		// meaningful to redirect a file write to; treat as not matching so
		// the caller's normal (chowned) path handles it, exactly as an
		// empty bootstrap file list would.
		return "", false
	}
	prefix := enforcedHooksHomePrefix + string(filepath.Separator)
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	return filepath.Join(enforcedHooksDir, rest), true
}

// clearEnforcedHooksDir removes every entry under hooks.EnforcedHooksDir
// (never the directory itself). handleBootstrap treats a non-nil return as a
// bootstrap failure — aborting before any file is written or init starts —
// so this is what makes "a hook removed since a previous bootstrap survives
// into this one" impossible: either the clear succeeds and stale content is
// gone before anything new is written, or it fails and the bootstrap never
// proceeds far enough for anything (stale or fresh) to run at all.
//
// It is symlink-safe the same way mkdirAllTracked's own trust model is
// documented to be: substrate-serve is the sole writer to this directory
// (nothing else in the actor has ever had a reason to write under it, since
// it is created and populated only by this file's own redirect), and this
// runs before the harness — and so the workload — ever starts, so there is
// no concurrent writer to race. A directory entry is recursed into and then
// rmdir'd; every other entry type, including a symlink, is unlinked
// directly and never followed.
//
// A missing hooks.EnforcedHooksDir (the common case: nothing has ever
// bootstrapped this actor before) is not an error.
func clearEnforcedHooksDir() error {
	return clearDirContents(enforcedHooksDir)
}

func clearDirContents(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return &bootstrapPathError{code: codeBootstrapPathSymlink, path: dir, detail: "hooks directory is a symlink; refusing to clear it"}
	}
	if !info.IsDir() {
		return &bootstrapPathError{code: codeBootstrapPathInvalid, path: dir, detail: "hooks directory is not a directory"}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() {
			// e.IsDir() is false for a symlink entry regardless of what it
			// points at (it reflects the directory entry's own d_type, not
			// a followed stat), so this branch only ever recurses into a
			// real, non-symlinked directory.
			if err := clearDirContents(p); err != nil {
				return err
			}
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		// Regular file or symlink entry: unlinked directly either way,
		// never followed.
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
