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

package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

// outDir writes files under one directory through an os.Root, which never
// resolves a path outside the directory, and refuses any symbolic link
// below it.
type outDir struct {
	root *os.Root
}

func openOutDir(dir string) (*outDir, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &outDir{root: r}, nil
}

func (d *outDir) Close() error { return d.root.Close() }

// WriteFile writes the slash-separated path rel under the directory; see
// the unix version.
func (d *outDir) WriteFile(rel string, replace bool, write func(io.Writer) error) error {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		p := strings.Join(parts[:i], "/")
		st, err := d.root.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			if err := d.root.Mkdir(p, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode()&fs.ModeSymlink != 0 || !st.IsDir() {
			return fmt.Errorf("%s is a symbolic link or not a folder; not writing through it", p)
		}
	}
	if _, err := d.root.Lstat(rel); err == nil && !replace {
		return fmt.Errorf("%s already exists; use --force to replace it", rel)
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmpName := path.Join(path.Dir(rel), ".artifact-"+hex.EncodeToString(rnd[:]))
	tmp, err := d.root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = d.root.Remove(tmpName) }()
	if err := write(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !replace {
		if _, err := d.root.Lstat(rel); err == nil {
			return fmt.Errorf("%s already exists; use --force to replace it", rel)
		}
	}
	return d.root.Rename(tmpName, rel)
}
