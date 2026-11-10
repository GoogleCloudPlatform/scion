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
	"os"
	"path"
)

// WriteAttachment writes r as attachment file name of channel and id into
// the resolved shared dir of Mount m, at
// <resolved>/.attachments/<channel>/<id>/<name>, and returns the file's
// container path (AttachmentContainerPath). Every operation goes through
// resolved.Open(create): the shared dir is agent-writable, so a component
// below it may be a planted symlink, and os.Root never follows one out of
// the directory. The file is created with O_EXCL, so an existing name
// (including a planted link) is an error, and a failed copy leaves no file.
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

	if err := root.MkdirAll(rel, 0o755); err != nil {
		return "", fmt.Errorf("create attachment dir: %w", err)
	}
	dest := path.Join(rel, name)
	f, err := root.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return "", fmt.Errorf("create attachment file: %w", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = root.Remove(dest)
		return "", fmt.Errorf("write attachment file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(dest)
		return "", fmt.Errorf("write attachment file: %w", err)
	}
	return containerPath, nil
}
