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

//go:build unix

package projectfs

import (
	"fmt"
	"io"

	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

// WriteAttachment writes r as attachment file name of channel and id into
// the resolved shared dir of Mount m, at
// <resolved>/.attachments/<channel>/<id>/<name>, and returns the file's
// container path (AttachmentContainerPath). Parent directories are created and
// pinned via rootopen.MkdirAll (no-follow component walk), and file creation
// and failure cleanup operate relative to that pinned leaf directory
// descriptor via rootopen.OpenFileForWriteInDir and rootopen.UnlinkInDir, so
// symlink components and existing entries are refused and a failed copy leaves
// no file.
func WriteAttachment(resolved Resolved, create bool, m Mount, channel, id, name string, r io.Reader) (string, error) {
	rel, err := AttachmentRelDir(channel, id)
	if err != nil {
		return "", err
	}
	containerPath, err := AttachmentContainerPath(m, channel, id, name)
	if err != nil {
		return "", err
	}
	root, err := resolved.Open(create)
	if err != nil {
		return "", fmt.Errorf("open shared dir %q: %w", m.Name, err)
	}
	defer func() { _ = root.Close() }()

	leafDir, err := rootopen.MkdirAll(root, rel, 0o755)
	if err != nil {
		return "", fmt.Errorf("create attachment dir: %w", err)
	}
	defer func() { _ = leafDir.Close() }()

	f, err := rootopen.OpenFileForWriteInDir(leafDir, name, 0o644, false)
	if err != nil {
		return "", fmt.Errorf("create attachment file: %w", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = rootopen.UnlinkInDir(leafDir, name)
		return "", fmt.Errorf("write attachment file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = rootopen.UnlinkInDir(leafDir, name)
		return "", fmt.Errorf("write attachment file: %w", err)
	}
	return containerPath, nil
}
