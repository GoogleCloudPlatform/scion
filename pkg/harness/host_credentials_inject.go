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

package harness

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// shippedRequiredFile identifies a required_files entry by the values that
// decide what host file is read and where it lands: secret name, AuthConfig
// field and home-relative target path.
type shippedRequiredFile struct {
	name, field, rel string
}

var (
	shippedRequiredFilesOnce sync.Once
	shippedRequiredFilesSet  map[shippedRequiredFile]struct{}
)

// shippedRequiredFiles returns the required_files entries declared by the
// harness configs embedded in the scion binary (harnesses/<name>/config.yaml).
// It is computed once; the embedded configs never change at run time.
func shippedRequiredFiles() map[shippedRequiredFile]struct{} {
	shippedRequiredFilesOnce.Do(func() {
		shippedRequiredFilesSet = make(map[shippedRequiredFile]struct{})
		for _, name := range AllHarnessNames() {
			data, err := fs.ReadFile(harnessesEmbed.FS, name+"/config.yaml")
			if err != nil {
				continue
			}
			entry, err := config.ParseHarnessConfigYAML(data)
			if err != nil || entry.Auth == nil {
				continue
			}
			for _, authType := range entry.Auth.Types {
				for _, rf := range authType.RequiredFiles {
					rel, ok := homeRelativeSuffix(rf.TargetSuffix)
					if rf.Name == "" || rf.Field == "" || !ok {
						continue
					}
					shippedRequiredFilesSet[shippedRequiredFile{rf.Name, rf.Field, rel}] = struct{}{}
				}
			}
		}
	})
	return shippedRequiredFilesSet
}

// InjectableHostCredentialFiles is the subset of HostCredentialFiles that a
// co-located workstation broker may copy into an agent. On top of the
// lexical home check in HostCredentialFiles, a file must:
//   - match, by name, field and target path, a required_files entry declared
//     by a harness config shipped with scion (embedded in the binary).
//     Entries that only a template-bundled, project or user-installed harness
//     config declares are never injected, so such a config cannot name an
//     arbitrary home file (for example an SSH key) and have it copied;
//   - resolve, after following symlinks, to a regular file inside the
//     (symlink-resolved) home directory.
//
// The returned Path is the resolved path. Files that are missing or fail a
// check are skipped (logged at debug, by name only).
func InjectableHostCredentialFiles(authMeta *config.HarnessAuthMetadata, home string) []HostCredentialFile {
	files := HostCredentialFiles(authMeta, home)
	if len(files) == 0 {
		return nil
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		util.Debugf("host credentials: cannot resolve home directory: %v", err)
		return nil
	}
	shipped := shippedRequiredFiles()
	var result []HostCredentialFile
	for _, f := range files {
		rel, ok := homeRelativeSuffix(f.TargetSuffix)
		if !ok {
			continue
		}
		if _, ok := shipped[shippedRequiredFile{f.Name, f.Field, rel}]; !ok {
			util.Debugf("host credentials: skipping %q: not declared by a harness config shipped with scion", f.Name)
			continue
		}
		resolved, err := filepath.EvalSymlinks(f.Path)
		if err != nil {
			continue
		}
		if !pathWithin(realHome, resolved) {
			util.Debugf("host credentials: skipping %q: resolves outside the home directory", f.Name)
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			util.Debugf("host credentials: skipping %q: not a regular file", f.Name)
			continue
		}
		f.Path = resolved
		result = append(result, f)
	}
	return result
}

// pathWithin reports whether path lies strictly below dir. Both must be
// absolute and already symlink-resolved.
func pathWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != "." && filepath.IsLocal(rel)
}

// maxHostCredentialFileSize bounds how much of a host credential file is
// read. Harness login files are a few KiB.
const maxHostCredentialFileSize = 1 << 20

// ReadInjectableHostCredentialFile reads a file returned by
// InjectableHostCredentialFiles while closing the gap between that check and
// the read: it opens the file, then re-resolves the declared path under home
// and requires that the resolved path is still strictly inside the resolved
// home and names the very file that was opened (same device and inode), and
// that it is a regular file no larger than 1 MiB. The content is read from
// the already-open descriptor.
func ReadInjectableHostCredentialFile(f HostCredentialFile, home string) ([]byte, error) {
	rel, ok := homeRelativeSuffix(f.TargetSuffix)
	if !ok {
		return nil, fmt.Errorf("target_suffix escapes the home directory")
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(f.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if opened.Size() > maxHostCredentialFileSize {
		return nil, fmt.Errorf("file larger than %d bytes", maxHostCredentialFileSize)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(home, rel))
	if err != nil {
		return nil, err
	}
	if !pathWithin(realHome, resolved) {
		return nil, fmt.Errorf("resolves outside the home directory")
	}
	current, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(opened, current) {
		return nil, fmt.Errorf("file changed while it was being read")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxHostCredentialFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxHostCredentialFileSize {
		return nil, fmt.Errorf("file larger than %d bytes", maxHostCredentialFileSize)
	}
	return data, nil
}
