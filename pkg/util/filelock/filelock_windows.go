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

//go:build windows

package filelock

// Windows implementation: CreateFile with FILE_FLAG_OPEN_REPARSE_POINT,
// a handle-based check that the leaf is a regular file, and LockFileEx on
// one byte at offset 0.
//
// WHY THE LOCK PATH IS NOT WALKED. Acquire opens only the leaf no-follow,
// by path; it does not walk the parent components handle by handle as a
// privatefs Tree does. That is acceptable ONLY while all three of these
// dependencies hold (keep the numbering, so that a later change can name
// which one failed):
//
//  1. Verified tree (tunnelclient: CheckPrivate on the state directory
//     handle). There privatefs has verified the directory owner-only, so
//     the only principals who could plant a junction above the leaf are
//     the user, SYSTEM and Administrators, who are outside this threat
//     model. cli_context does not verify an existing global directory
//     (it only creates missing components private) and relies on 2 and
//     3, as settings.lock does.
//  2. Leaf no-follow. The leaf itself is opened with
//     FILE_FLAG_OPEN_REPARSE_POINT and checked on the handle
//     (FileAttributeTagInfo): any reparse point or directory is refused
//     with ErrNotRegular.
//  3. No secret behind the lock. A lock gates no secret; it only
//     serialises writers. Settings and broker-credential locks protect
//     write ordering, not confidentiality.
//
// If any one of these stops holding, the conclusion changes and the lock
// must move onto a privatefs Tree handle walk. Locks outside any private
// tree (settings.lock in a non-private settings directory) rely on 2 and 3
// only, which is the same exposure as unix flock on a path.
//
// NO FILE_SHARE_DELETE. Acquire's handle shares
// read and write but not delete. With share-delete, another process could
// delete a held lock file and a third could CREATE_NEW and lock a new file
// of the same name, so two holders would each believe they had the lock.
// Not sharing delete enforces the package contract that a lock file is
// never removed while held.
//
// LockFile is the one exception: it locks a handle the caller opened, with
// whatever sharing the caller chose. Its doc comment warns that the caller
// owns the file's lifetime and that it must not be used for mutual
// exclusion on a shared name. Its only caller is tunnelclient, whose
// per-process lock files are a liveness signal on a random name, renamed
// and removed by their owner while held.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// tryLock makes one non-blocking LockFileEx attempt on byte 0 (lock files
// are empty; locking past the end of a file is legal). held reports that
// someone else holds the lock.
func tryLock(f *os.File) (held bool, err error) {
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	runtime.KeepAlive(f) // f.Fd() is valid only while f is reachable
	if err == nil {
		return false, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return true, nil
	}
	return false, err
}

// unlock releases the byte-range lock. The kernel also releases it when
// the handle is closed or the process exits.
func unlock(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
	runtime.KeepAlive(f)
}

const (
	lockShare      = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE // no FILE_SHARE_DELETE: a held lock file cannot be deleted
	noFollowCreate = windows.FILE_FLAG_OPEN_REPARSE_POINT | windows.FILE_ATTRIBUTE_NORMAL
	// FILE_FLAG_BACKUP_SEMANTICS lets an existing directory open, so it is
	// refused as ErrNotRegular on its handle rather than failing earlier
	// with an unrelated error.
	noFollowOpen = noFollowCreate | windows.FILE_FLAG_BACKUP_SEMANTICS
)

// open opens path for locking, creating it if needed. created reports
// whether this call created it.
func open(path string, mode os.FileMode) (f *os.File, created bool, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, fmt.Errorf("create lock file: %w", err)
	}
	sa, err := privatefs.SecurityAttributesFor(privatefs.ForMode(mode))
	if err != nil {
		return nil, false, fmt.Errorf("create lock file: %w", err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, lockShare, sa,
		windows.CREATE_NEW, noFollowCreate, 0)
	if err == nil {
		return os.NewFile(uintptr(h), path), true, nil
	}
	// ERROR_ACCESS_DENIED here can also mean the name is a directory.
	if !errors.Is(err, windows.ERROR_FILE_EXISTS) && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) &&
		!errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil, false, fmt.Errorf("create lock file: %w", &fs.PathError{Op: "open", Path: path, Err: err})
	}
	createErr := err
	f, err = openNoFollow(path, p, lockShare)
	if err != nil && errors.Is(createErr, windows.ERROR_ACCESS_DENIED) && errors.Is(err, fs.ErrNotExist) {
		// The create was denied (the directory is not writable) and there
		// is nothing to open: report the denial, not "not found".
		return nil, false, fmt.Errorf("create lock file: %w", &fs.PathError{Op: "open", Path: path, Err: createErr})
	}
	return f, false, err
}

// openNoFollow opens an existing path without following a reparse point,
// falling back to read-only access (LockFileEx works on a read handle, as
// flock does on a read-only descriptor), and refuses anything but a
// regular file on the handle.
func openNoFollow(path string, p *uint16, share uint32) (*os.File, error) {
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, share, nil,
		windows.OPEN_EXISTING, noFollowOpen, 0)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		h, err = windows.CreateFile(p, windows.GENERIC_READ, share, nil, windows.OPEN_EXISTING, noFollowOpen, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", &fs.PathError{Op: "open", Path: path, Err: err})
	}
	info, tagKnown, err := attrTag(h)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("stat lock file: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("%w: %s is a symlink, junction or other reparse point%s", ErrNotRegular, path, reparseTagText(info.ReparseTag, tagKnown))
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("%w: %s is a directory", ErrNotRegular, path)
	}
	return os.NewFile(uintptr(h), path), nil
}

type fileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

// attrTag reads the attributes (and reparse tag) of the open handle h with
// FileAttributeTagInfo. Only ERROR_INVALID_PARAMETER falls back: that is
// what FAT32, which has no reparse points, answers for this information
// class. Any other error fails closed. (privatefs has the same helper; the
// two packages share no internal package.) tagKnown is false after the
// fallback, which reads no reparse tag: info.ReparseTag is then 0 because
// it is unknown, not because the file has tag 0.
func attrTag(h windows.Handle) (info fileAttributeTagInfo, tagKnown bool, err error) {
	err = windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		attrs, ferr := attrsByHandleInfo(h)
		if ferr != nil {
			return info, false, fmt.Errorf("%w (FileAttributeTagInfo: %v)", ferr, err)
		}
		return fileAttributeTagInfo{FileAttributes: attrs}, false, nil
	}
	return info, err == nil, err
}

// reparseTagText describes a reparse tag for an error message. It claims a
// tag only when one was read (tagKnown); the FAT attribute fallback reads
// none.
func reparseTagText(tag uint32, tagKnown bool) string {
	if !tagKnown {
		return " (its reparse tag was not read)"
	}
	return fmt.Sprintf(" (tag %#x)", tag)
}

// attrsByHandleInfo reads the attributes of the open handle h with
// GetFileInformationByHandle. They carry the same FILE_ATTRIBUTE_REPARSE_POINT
// and FILE_ATTRIBUTE_DIRECTORY bits as FileAttributeTagInfo on any file
// system; only the reparse tag is missing, and no decision reads the tag.
func attrsByHandleInfo(h windows.Handle) (uint32, error) {
	var bhfi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &bhfi); err != nil {
		return 0, err
	}
	return bhfi.FileAttributes, nil
}
