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

//go:build !unix

package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agentplugin"
)

// Writes into an existing agent home, which the agent's container can
// change, confined to the home (the pattern writeTaskFile uses): every
// path is resolved inside an os.Root opened on the home, every parent
// directory must be a real directory (checked with Lstat, never followed),
// and a file or directory is installed by renaming a freshly created
// temporary entry over the target, so an entry the container put at the
// target is replaced, never written through.

// homeDirChain makes rel (slash or OS separated, relative to the home) a
// chain of real directories inside home: an existing component that is
// not a real directory is refused, missing ones are created.
func homeDirChain(home *os.Root, rel string) error {
	rel = filepath.Clean(filepath.FromSlash(rel))
	if rel == "." {
		return nil
	}
	if ok, err := realDirChain(home, rel, false); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%s in the agent home is not a directory", rel)
	}
	if err := home.MkdirAll(rel, 0o755); err != nil {
		return err
	}
	if ok, err := realDirChain(home, rel, true); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%s in the agent home changed while it was created", rel)
	}
	return nil
}

// tempHomeName returns a new hidden name next to base, for an entry that
// is renamed over base once complete.
func tempHomeName(base string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return "." + base + "." + hex.EncodeToString(suffix[:]), nil
}

// writeHomeFile writes data to rel inside the agent home at agentHome,
// confined to the home: the parent directories must be real directories
// (created when missing), and the file is written to a new temporary file
// and renamed over rel, so whatever is at rel is replaced, not followed.
func writeHomeFile(agentHome, rel string, data []byte, mode os.FileMode) error {
	home, err := os.OpenRoot(agentHome)
	if err != nil {
		return fmt.Errorf("open the agent home: %w", err)
	}
	defer func() { _ = home.Close() }()
	return writeRootFile(home, rel, data, mode)
}

func writeRootFile(home *os.Root, rel string, data []byte, mode os.FileMode) error {
	rel = filepath.Clean(filepath.FromSlash(rel))
	if rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return fmt.Errorf("invalid agent home path %q", rel)
	}
	parent, base := filepath.Dir(rel), filepath.Base(rel)
	if err := homeDirChain(home, parent); err != nil {
		return err
	}
	dir, err := home.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	// The directory opened must be the real one checked above, not a link
	// put in its place since.
	checked, err := home.Lstat(parent)
	if err != nil {
		return err
	}
	if opened, err := dir.Stat("."); err != nil || !os.SameFile(checked, opened) {
		return fmt.Errorf("%s in the agent home changed while it was opened", parent)
	}
	tmp, err := tempHomeName(base)
	if err != nil {
		return err
	}
	f, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			_ = dir.Remove(tmp)
		}
	}()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Chmod(mode)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	// A directory at the target cannot be replaced by a rename; refuse it
	// rather than removing what the container put there.
	if fi, err := dir.Lstat(base); err == nil && fi.IsDir() {
		return fmt.Errorf("%s in the agent home is a directory", rel)
	}
	if err := dir.Rename(tmp, base); err != nil {
		return err
	}
	installed = true
	return nil
}

// removeHomeFile removes rel inside the agent home when it is there,
// confined to the home: an entry at rel that is a symbolic link is
// removed itself, never its target, and a missing parent means there is
// nothing to remove. A directory at rel is left alone.
func removeHomeFile(agentHome, rel string) error {
	home, err := os.OpenRoot(agentHome)
	if err != nil {
		return err
	}
	defer func() { _ = home.Close() }()
	rel = filepath.Clean(filepath.FromSlash(rel))
	parent := filepath.Dir(rel)
	if parent != "." {
		ok, err := realDirChain(home, parent, true)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	fi, err := home.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return nil
	}
	if err := home.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// installHomeDir replaces the directory dstRel inside home with a copy of
// the directory srcRel inside src (regular files and directories only,
// within agentplugin.DefaultLimits), confined to the home: the copy is
// made under a temporary name next to dstRel, whose parents must be real
// directories, then whatever is at dstRel is removed (a link itself, not
// its target) and the copy renamed into place.
func installHomeDir(srcDir, agentHome, dstRel string) error {
	src, err := os.OpenRoot(srcDir)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	home, err := os.OpenRoot(agentHome)
	if err != nil {
		return err
	}
	defer func() { _ = home.Close() }()
	srcRel := "."
	dstRel = filepath.Clean(filepath.FromSlash(dstRel))
	parent, base := filepath.Dir(dstRel), filepath.Base(dstRel)
	if err := homeDirChain(home, parent); err != nil {
		return err
	}
	tmpBase, err := tempHomeName(base)
	if err != nil {
		return err
	}
	tmp := filepath.Join(parent, tmpBase)
	if _, err := agentplugin.CopyTree(src, filepath.FromSlash(srcRel), home, tmp, agentplugin.DefaultLimits); err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			_ = home.RemoveAll(tmp)
		}
	}()
	if ok, err := realDirChain(home, parent, true); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%s in the agent home changed during the copy", parent)
	}
	if err := home.RemoveAll(dstRel); err != nil {
		return err
	}
	if err := home.Rename(tmp, dstRel); err != nil {
		return err
	}
	installed = true
	return nil
}
