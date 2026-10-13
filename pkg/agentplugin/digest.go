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

package agentplugin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

// TreeDigest returns "sha256:<hex>" over the sorted list of
// (relpath, isExec, sha256(content)) for every regular file under rel, plus
// directory relpaths. Symlinks and special files are ignored (CopyTree never
// copies them). The result is deterministic across filesystems.
func TreeDigest(root *os.Root, rel string) (string, error) {
	fi, err := root.Lstat(rel)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", rel)
	}
	var records []string
	var walk func(dir, relDir string) error
	walk = func(dir, relDir string) error {
		entries, err := readRootDir(root, dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			r := filepath.ToSlash(filepath.Join(relDir, e.Name()))
			efi, err := root.Lstat(p)
			if err != nil {
				return err
			}
			switch {
			case efi.IsDir():
				records = append(records, "d\x00"+r)
				if err := walk(p, r); err != nil {
					return err
				}
			case efi.Mode().IsRegular():
				sum, err := fileSHA256(root, p)
				if err != nil {
					return err
				}
				exec := "0"
				if efi.Mode().Perm()&0o111 != 0 {
					exec = "1"
				}
				records = append(records, "f\x00"+r+"\x00"+exec+"\x00"+sum)
			}
		}
		return nil
	}
	if err := walk(rel, ""); err != nil {
		return "", err
	}
	slices.Sort(records)
	h := sha256.New()
	for _, r := range records {
		_, _ = io.WriteString(h, r)
		_, _ = h.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func fileSHA256(root *os.Root, name string) (string, error) {
	f, err := root.Open(name)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
