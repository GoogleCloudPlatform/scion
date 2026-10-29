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

package projectfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Removal of attachment copies from a shared dir. The shared dir is
// agent-writable, so every component below it may be a planted symlink:
// .attachments and the channel directory are Lstat'ed without following
// and walked only when they are real directories, each opened as its own
// os.Root whose identity must equal what was Lstat'ed. Every removal is
// relative to that descriptor, so nothing outside the channel directory is
// ever touched. On platforms where Resolved.Open is unsupported these
// functions fail with errors.ErrUnsupported.

// ErrNotRealDir reports that .attachments or a channel directory is not a
// real directory (a symlink or a file), so it was not walked.
var ErrNotRealDir = errors.New("projectfs: not a real directory")

// RemoveAttachmentChannel removes <resolved>/.attachments/<channel> and
// everything below it. channel must be a single safe path component. It
// reports whether anything was removed; a missing shared dir, .attachments
// or channel is not an error (the call is idempotent). When .attachments or
// the channel is not a real directory, nothing is removed and the error
// wraps ErrNotRealDir.
func RemoveAttachmentChannel(resolved Resolved, channel string) (bool, error) {
	if err := validAttachmentComponent("channel", channel); err != nil {
		return false, err
	}
	attachments, err := openAttachmentsDir(resolved)
	if attachments == nil {
		return false, err
	}
	defer func() { _ = attachments.Close() }()
	ch, err := openRealSubdir(attachments, channel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	_ = ch.Close()
	if err := attachments.RemoveAll(channel); err != nil {
		return false, fmt.Errorf("remove attachment channel %q: %w", channel, err)
	}
	return true, nil
}

// RemoveAttachmentEntry removes <resolved>/.attachments/<channel>/<entry>
// (one attachment's directory, or a file) and everything below it. channel
// and entry must each be a single safe path component. An entry that is a
// symlink is removed as the link itself; its target is never touched. It
// reports whether anything was removed; a missing shared dir,
// .attachments, channel or entry is not an error (the call is idempotent).
// When .attachments or the channel is not a real directory, nothing is
// removed and the error wraps ErrNotRealDir.
func RemoveAttachmentEntry(resolved Resolved, channel, entry string) (bool, error) {
	if err := validAttachmentComponent("channel", channel); err != nil {
		return false, err
	}
	if err := validAttachmentComponent("entry", entry); err != nil {
		return false, err
	}
	attachments, err := openAttachmentsDir(resolved)
	if attachments == nil {
		return false, err
	}
	defer func() { _ = attachments.Close() }()
	ch, err := openRealSubdir(attachments, channel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer func() { _ = ch.Close() }()
	if _, err := ch.Lstat(entry); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("check attachment entry: %w", err)
	}
	if err := ch.RemoveAll(entry); err != nil {
		return false, fmt.Errorf("remove attachment entry: %w", err)
	}
	return true, nil
}

// ListAttachmentEntries returns the names of the entries of
// <resolved>/.attachments/<channel>. A missing shared dir, .attachments or
// channel gives no names and no error. When .attachments or the channel is
// not a real directory, the error wraps ErrNotRealDir.
func ListAttachmentEntries(resolved Resolved, channel string) ([]string, error) {
	if err := validAttachmentComponent("channel", channel); err != nil {
		return nil, err
	}
	attachments, err := openAttachmentsDir(resolved)
	if attachments == nil {
		return nil, err
	}
	defer func() { _ = attachments.Close() }()
	ch, err := openRealSubdir(attachments, channel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = ch.Close() }()
	f, err := ch.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open attachment channel: %w", err)
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("list attachment channel: %w", err)
	}
	return names, nil
}

// openAttachmentsDir opens the shared dir of resolved (without creating it)
// and its .attachments directory as a real directory. It returns a nil
// root and a nil error when either does not exist.
func openAttachmentsDir(resolved Resolved) (*os.Root, error) {
	root, err := resolved.Open(false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = root.Close() }()
	attachments, err := openRealSubdir(root, AttachmentsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return attachments, nil
}

// openRealSubdir opens name, a single component inside parent, as an
// os.Root only when it is a real directory: Lstat (never following) must
// report a directory, and the opened root must be that same directory
// (os.SameFile), so a name swapped for a link between the two is refused.
func openRealSubdir(parent *os.Root, name string) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s: %w", name, ErrNotRealDir)
	}
	sub, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := sub.Stat(".")
	if err != nil {
		_ = sub.Close()
		return nil, err
	}
	if !os.SameFile(info, opened) {
		_ = sub.Close()
		return nil, fmt.Errorf("%s: %w", name, ErrNotRealDir)
	}
	return sub, nil
}
