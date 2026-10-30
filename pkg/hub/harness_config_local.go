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

package hub

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// The harness-config bootstrap and import read a directory's config.yaml
// before the directory is collected: to pick the harness for the store, to
// bind the import name, and to check the provisioner. These reads follow the
// collection's rules, so they never see content the collection would not
// record: the directory must be a real directory, not a link
// (openRealDirRoot, shared with collectFiles), and each file is read only if
// it is a regular file, through an os.Root opened at the directory
// (transfer.OpenRegularFile, shared with transfer.OpenCollectedFile), so a
// link reads as absent and nothing outside the directory is opened.

// errNotRegularLocalFile is the sentinel for a harness-config file that is a
// link or not a regular file; errors.Is matches it on notRegularLocalFile.
var errNotRegularLocalFile = errors.New("a link or not a regular file")

// notRegularLocalFile reports a harness-config file that is a link or not a
// regular file. Such a file reads as absent, as in the collected set, so
// errors.Is matches fs.ErrNotExist as well as errNotRegularLocalFile; the
// message says what the file is.
type notRegularLocalFile struct{ name string }

func (e *notRegularLocalFile) Error() string {
	return e.name + " is a link or not a regular file"
}

func (e *notRegularLocalFile) Is(target error) bool {
	return target == errNotRegularLocalFile || target == fs.ErrNotExist
}

// errLocalHarnessConfigTooLarge reports a harness-config file read before
// collection that is larger than maxHarnessConfigFileSize.
var errLocalHarnessConfigTooLarge = fmt.Errorf("larger than the %d-byte harness-config file limit", maxHarnessConfigFileSize)

// readLocalHarnessConfigRootFile reads the top-level file name through root
// with transfer.OpenRegularFile: Lstat, a non-blocking open (a FIFO swapped
// in after the Lstat does not block), then fstat and SameFile on the opened
// file. A link or another non-regular file reads as absent
// (notRegularLocalFile). A file replaced between the Lstat and the open
// fails the read, and so does a file larger than maxHarnessConfigFileSize
// (errLocalHarnessConfigTooLarge; at most that many bytes plus one are read,
// as the hub's other harness-config file reads do).
func readLocalHarnessConfigRootFile(root *os.Root, name string) ([]byte, error) {
	f, _, err := transfer.OpenRegularFile(root, name)
	if errors.Is(err, transfer.ErrNotRegularFile) {
		return nil, &notRegularLocalFile{name: name}
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxHarnessConfigFileSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxHarnessConfigFileSize {
		return nil, fmt.Errorf("%s: %w", name, errLocalHarnessConfigTooLarge)
	}
	return data, nil
}

// readLocalHarnessConfigFile reads the top-level file name of the
// harness-config directory dir, confined as described above.
func readLocalHarnessConfigFile(dir, name string) ([]byte, error) {
	root, _, err := openRealDirRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return readLocalHarnessConfigRootFile(root, name)
}

// loadLocalHarnessConfigDir is config.LoadHarnessConfigDir for the reads
// before collection: the same validation, sidecar handling and name rules,
// over config.yaml and the sidecar read confined to dir. A config.yaml that
// is a link or not a regular file fails the load.
func loadLocalHarnessConfigDir(dir string) (*config.HarnessConfigDir, error) {
	absPath, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve path: %w", err)
	}
	root, _, err := openRealDirRoot(absPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	return config.LoadHarnessConfigFromFiles(filepath.Base(absPath), absPath, func(name string) ([]byte, error) {
		return readLocalHarnessConfigRootFile(root, name)
	})
}
