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

package privatefs

// This is the Windows implementation. It is new Windows code, not the POSIX
// sequence behind a build tag: it sets no mode bits and never calls Chmod
// (on Windows Chmod only toggles FILE_ATTRIBUTE_READONLY).
//
// Security descriptor:
//   - The owner is set explicitly to the token user's SID. Under an
//     elevated token the default owner is BUILTIN\Administrators, so an
//     implicit owner would make "owner-only" mean "admins-only".
//   - The DACL is protected (SE_DACL_PROTECTED), so no inherited ACE
//     survives, and grants FILE_ALL_ACCESS to the token user and SYSTEM
//     only. Administrators are not granted on create.
//   - Every SID is the token user's or well-known (CreateWellKnownSid,
//     StringToSid). No account name is ever looked up or compared: names
//     are localised and a local group can be given any name.
//   - The descriptor is passed in the create call (OBJECT_ATTRIBUTES or
//     SECURITY_ATTRIBUTES), so the object never exists without it. Only an
//     EXISTING file is ever re-secured (the in-place repair in
//     verifyOrRepair).
//
// Tree walk: every component below the anchor is opened with
// NtCreateFile relative to its parent's handle (OBJECT_ATTRIBUTES.
// RootDirectory) with FILE_OPEN_REPARSE_POINT, and its attributes are read
// on the new handle. A component carrying FILE_ATTRIBUTE_REPARSE_POINT, of
// any tag, is refused with ErrReparsePoint. Nothing below the anchor is
// ever resolved by path, so there is no check-then-open.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
)

func isSeparator(c byte) bool { return c == '/' || c == '\\' }

func isAbsLike(rel string) bool {
	return filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" || isSeparator(rel[0])
}

// Describe returns how p protects a file, for user-facing text.
func Describe(p Protection) string {
	if p == Private {
		return "owner-only ACL"
	}
	return "inherited ACL"
}

// identity is the process's token user and the security descriptors built
// from it, computed once.
type identity struct {
	user    *windows.SID
	system  *windows.SID
	admins  *windows.SID
	fileSD  *windows.SECURITY_DESCRIPTOR
	dirSD   *windows.SECURITY_DESCRIPTOR
	fileACL *windows.ACL // the DACL of fileSD, for in-place repair
}

var (
	identOnce sync.Once
	identVal  *identity
	identErr  error
)

func getIdentity() (*identity, error) {
	identOnce.Do(func() { identVal, identErr = newIdentity() })
	return identVal, identErr
}

func newIdentity() (*identity, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("read the token user: %w", err)
	}
	user, err := tu.User.Sid.Copy()
	if err != nil {
		return nil, err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	u, sy := user.String(), system.String()
	// FA = FILE_ALL_ACCESS. P = protected DACL. OICI = inherited by files
	// and subdirectories created inside the directory.
	fileSD, err := windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;%s)", u, u, sy))
	if err != nil {
		return nil, fmt.Errorf("build the private file security descriptor: %w", err)
	}
	dirSD, err := windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;OICI;FA;;;%s)(A;OICI;FA;;;%s)", u, u, sy))
	if err != nil {
		return nil, fmt.Errorf("build the private directory security descriptor: %w", err)
	}
	fileACL, _, err := fileSD.DACL()
	if err != nil {
		return nil, err
	}
	return &identity{user: user, system: system, admins: admins, fileSD: fileSD, dirSD: dirSD, fileACL: fileACL}, nil
}

// SecurityAttributesFor returns the SECURITY_ATTRIBUTES a CreateFile call
// passes to create a file with protection p: the private file descriptor
// for Private, nil (the parent's inherited DACL) for Inherit.
func SecurityAttributesFor(p Protection) (*windows.SecurityAttributes, error) {
	if p != Private {
		return nil, nil
	}
	id, err := getIdentity()
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: id.fileSD}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa, nil
}

// Tree is a private directory tree rooted at a trusted anchor. Links at or
// above the anchor are followed (a redirected profile is legitimate). Below
// it, no component may be a reparse point.
type Tree struct {
	anchor string         // pathcanon.Resolve output (\\?\ form), for messages
	h      windows.Handle // the anchor directory; pins the object
	acls   bool           // the volume has FILE_PERSISTENT_ACLS
	fsName string
	// followLinks: TreeOptions.FollowLinks (directory components below the
	// anchor may be reparse points, which are followed).
	followLinks bool
	// warn and warned: TreeOptions.Warn, and the directories already
	// reported.
	warn   func(string)
	warned map[string]bool
	// volSerial and fileIndex identify the anchor object. They are recorded
	// for diagnostics only (error text, tests); no security decision reads
	// them, because the handle itself pins the object.
	volSerial uint32
	fileIndex uint64
}

const (
	shareAll  = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	dirAccess = windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL

	// IO_STATUS_BLOCK.Information values for NtCreateFile.
	fileOpened  = 1
	fileCreated = 2
)

// OpenTree opens the tree rooted at anchor. The anchor is canonicalised
// with pathcanon.Resolve and opened following links; it must be a
// directory. With opt.CreateAnchor, missing components of the anchor are
// created, each relative to its parent's handle, with the private
// directory descriptor.
func OpenTree(anchor string, opt TreeOptions) (*Tree, error) {
	if _, err := getIdentity(); err != nil {
		return nil, err
	}
	resolved, err := pathcanon.Resolve(anchor)
	if err != nil {
		return nil, err
	}
	h, err := openFollow(resolved)
	if err != nil && opt.CreateAnchor && errors.Is(err, fs.ErrNotExist) {
		h, err = createAnchor(resolved)
	}
	if err != nil {
		return nil, err
	}
	t := &Tree{anchor: resolved, h: h, followLinks: opt.FollowLinks, warn: opt.Warn}
	if err := t.init(); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	return t, nil
}

func (t *Tree) init() error {
	tag, err := attrTag(t.h)
	if err != nil {
		return &fs.PathError{Op: "open tree", Path: t.anchor, Err: err}
	}
	if tag.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return &fs.PathError{Op: "open tree", Path: t.anchor, Err: syscall.ENOTDIR}
	}
	acls, fsName, err := volumeACLs(t.h)
	if err != nil {
		return &fs.PathError{Op: "open tree", Path: t.anchor, Err: fmt.Errorf("read volume information: %w", err)}
	}
	t.acls, t.fsName = acls, fsName
	var bhfi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(t.h, &bhfi); err == nil {
		t.volSerial = bhfi.VolumeSerialNumber
		t.fileIndex = uint64(bhfi.FileIndexHigh)<<32 | uint64(bhfi.FileIndexLow)
	}
	return nil
}

// volumeACLs reports whether the volume holding h stores ACLs.
func volumeACLs(h windows.Handle) (bool, string, error) {
	var flags uint32
	name := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeInformationByHandle(h, nil, 0, nil, nil, &flags, &name[0], uint32(len(name))); err != nil {
		return false, "", err
	}
	return flags&windows.FILE_PERSISTENT_ACLS != 0, windows.UTF16ToString(name), nil
}

// openFollow opens an existing directory by path, following links. It is
// used only for the anchor and its ancestors.
func openFollow(path string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	h, err := windows.CreateFile(p, dirAccess|windows.SYNCHRONIZE, shareAll, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return windows.InvalidHandle, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return h, nil
}

// createAnchor creates the missing components of the anchor path below its
// deepest existing ancestor, each relative to its parent's handle, with the
// private directory descriptor, and returns a handle on the anchor.
func createAnchor(path string) (windows.Handle, error) {
	var missing []string
	cur := path
	var h windows.Handle
	for {
		var err error
		h, err = openFollow(cur)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return windows.InvalidHandle, err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return windows.InvalidHandle, err
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
	acls, fsName, err := volumeACLs(h)
	if err != nil {
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, err
	}
	if !acls {
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, noACLsError(path, fsName)
	}
	id, err := getIdentity()
	if err != nil {
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		name := missing[i]
		child, _, err := ntCreate(h, name, dirAccess, 0, shareAll, windows.FILE_CREATE,
			windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT, id.dirSD)
		if errors.Is(err, fs.ErrExist) {
			// Created concurrently: open it, still refusing a reparse point.
			child, err = openDirAt(h, name)
		}
		_ = windows.CloseHandle(h)
		if err != nil {
			return windows.InvalidHandle, &fs.PathError{Op: "mkdir", Path: path, Err: err}
		}
		h = child
	}
	return h, nil
}

func noACLsError(anchor, fsName string) error {
	return fmt.Errorf("cannot store private file under %s: the volume (%s) does not support access control lists, so the file cannot be protected: %w", anchor, fsName, ErrNoACLs)
}

// Anchor returns the tree's anchor (the pathcanon.Resolve form).
func (t *Tree) Anchor() string { return t.anchor }

// Close releases the anchor handle.
func (t *Tree) Close() error {
	if t.h == windows.InvalidHandle || t.h == 0 {
		return nil
	}
	err := windows.CloseHandle(t.h)
	t.h = windows.InvalidHandle
	return err
}

func (t *Tree) display(rel string) string {
	return strings.TrimRight(t.anchor, `\`) + `\` + strings.ReplaceAll(rel, "/", `\`)
}

func (t *Tree) diag() string {
	return fmt.Sprintf("tree %s (volume %08x, file %016x)", t.anchor, t.volSerial, t.fileIndex)
}

func (t *Tree) reparseErr(rel string) error {
	return fmt.Errorf("refusing %s: %w; %s", t.display(rel), ErrReparsePoint, t.diag())
}

func (t *Tree) checkACLs(p Protection) error {
	if p == Private && !t.acls {
		return noACLsError(t.anchor, t.fsName)
	}
	return nil
}

// ntCreate is NtCreateFile of name relative to the directory root, for
// synchronous I/O, case-insensitive like Win32. sd, when not nil, is the
// security descriptor of a newly created object.
func ntCreate(root windows.Handle, name string, access, attrs, share, disposition, options uint32, sd *windows.SECURITY_DESCRIPTOR) (windows.Handle, uintptr, error) {
	u, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, 0, err
	}
	oa := windows.OBJECT_ATTRIBUTES{
		RootDirectory:      root,
		ObjectName:         u,
		Attributes:         windows.OBJ_CASE_INSENSITIVE,
		SecurityDescriptor: sd,
	}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var iosb windows.IO_STATUS_BLOCK
	var h windows.Handle
	err = windows.NtCreateFile(&h, access|windows.SYNCHRONIZE, &oa, &iosb, nil, attrs, share,
		disposition, options|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return windows.InvalidHandle, 0, ntErr(err)
	}
	return h, iosb.Information, nil
}

// ntErr converts an NTSTATUS into the Win32 error the os package uses, so
// errors.Is(err, fs.ErrNotExist) and fs.ErrExist work.
func ntErr(err error) error {
	st, ok := err.(windows.NTStatus)
	if !ok {
		return err
	}
	switch st {
	case windows.STATUS_REPARSE_POINT_ENCOUNTERED:
		return ErrReparsePoint
	case windows.STATUS_OBJECT_NAME_COLLISION:
		return windows.ERROR_FILE_EXISTS
	}
	return st.Errno()
}

type fileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

// attrTag reads the attributes (and reparse tag) of the open handle h with
// FileAttributeTagInfo. Only ERROR_INVALID_PARAMETER falls back: that is
// what FAT32, which has no reparse points, answers for this information
// class. Any other error fails closed.
func attrTag(h windows.Handle) (fileAttributeTagInfo, error) {
	var info fileAttributeTagInfo
	err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		attrs, ferr := attrsByHandleInfo(h)
		if ferr != nil {
			return info, fmt.Errorf("read file attributes: %w (FileAttributeTagInfo: %v)", ferr, err)
		}
		return fileAttributeTagInfo{FileAttributes: attrs}, nil
	}
	if err != nil {
		return info, fmt.Errorf("read file attributes: %w", err)
	}
	return info, nil
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

// isReparse reports whether h is a reparse point of any tag.
func isReparse(h windows.Handle) (bool, fileAttributeTagInfo, error) {
	tag, err := attrTag(h)
	if err != nil {
		return false, tag, err
	}
	return tag.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, tag, nil
}

// openDirAt opens the directory name relative to parent without following
// a reparse point, and refuses it if it is one or is not a directory.
func openDirAt(parent windows.Handle, name string) (windows.Handle, error) {
	h, _, err := ntCreate(parent, name, dirAccess, 0, shareAll, windows.FILE_OPEN, windows.FILE_OPEN_REPARSE_POINT, nil)
	if err != nil {
		return windows.InvalidHandle, err
	}
	rp, tag, err := isReparse(h)
	switch {
	case err != nil:
	case rp:
		err = ErrReparsePoint
	case tag.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0:
		err = syscall.ENOTDIR
	}
	if err != nil {
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, err
	}
	return h, nil
}

// openDirComp opens the existing directory component name relative to
// parent. In a tree without FollowLinks it is openDirAt: a reparse point is
// refused.
//
// With FollowLinks (see TreeOptions.FollowLinks for why ~/.ssh needs it):
//   - a reparse point is opened again without FILE_OPEN_REPARSE_POINT, so
//     the kernel follows it (relative to parent's handle, never by a path
//     built here), and the result must be a directory;
//   - every EXISTING component, followed or not, must pass
//     CheckNoForeignWriters on its handle (the follow-then-verify owner
//     set: the
//     current user, SYSTEM or Administrators). A directory someone else can
//     write to or delete children from could have its entries swapped after
//     any check made below it, so it is refused before anything is written
//     there.
//     The only components that skip the check are the ones mkdirAt
//     creates during the current walk (they get the protection asked for
//     in the create call, and walk and MkdirAll use the new handle without
//     coming back here). The carve-out keys on "created by this walk",
//     never on a name: a pre-existing directory of any name, including
//     one created concurrently (mkdirAt's ErrExist path), comes through
//     here and is checked. The anchor itself is trusted, as for every
//     Tree.
func (t *Tree) openDirComp(parent windows.Handle, name string) (windows.Handle, error) {
	h, err := openDirAt(parent, name)
	if !t.followLinks {
		return h, err
	}
	if errors.Is(err, ErrReparsePoint) {
		h, _, err = ntCreate(parent, name, dirAccess, 0, shareAll, windows.FILE_OPEN, windows.FILE_DIRECTORY_FILE, nil)
		if err != nil {
			return windows.InvalidHandle, err
		}
		tag, terr := attrTag(h)
		if terr == nil && tag.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			terr = syscall.ENOTDIR
		}
		if terr != nil {
			_ = windows.CloseHandle(h)
			return windows.InvalidHandle, terr
		}
		if err == nil {
			t.warnMutableLinkParent(h, name)
		}
	}
	if err != nil {
		return windows.InvalidHandle, err
	}
	if err := checkHandleNoForeignWriters(h, name); err != nil {
		_ = windows.CloseHandle(h)
		return windows.InvalidHandle, err
	}
	return h, nil
}

// warnMutableLinkParent reports, through TreeOptions.Warn, a followed
// link whose target directory (open as h) sits in a parent directory that
// another principal can write to or delete children from: the accepted
// residual of FollowLinks (see TreeOptions.FollowLinks). It
// is DIAGNOSTIC ONLY: it never changes a result, so the parent is opened
// by path (GetFinalPathNameByHandle of h, then its directory), which no
// decision on the walk ever does.
func (t *Tree) warnMutableLinkParent(h windows.Handle, name string) {
	if t.warn == nil {
		return
	}
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
	if err == nil && int(n) > len(buf) {
		buf = make([]uint16, n)
		n, err = windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
	}
	if err != nil || int(n) > len(buf) {
		return
	}
	target := windows.UTF16ToString(buf[:n])
	parent := filepath.Dir(target)
	if parent == target || t.warned[parent] {
		return
	}
	ph, err := openFollow(parent)
	if err != nil {
		return
	}
	defer func() { _ = windows.CloseHandle(ph) }()
	if cerr := checkHandleNoForeignWriters(ph, parent); errors.Is(cerr, ErrNotPrivate) {
		if t.warned == nil {
			t.warned = map[string]bool{}
		}
		t.warned[parent] = true
		t.warn(fmt.Sprintf("%s is a link to %s, and the directory above it, %s, can be changed by another user (%v); scion protects the files it writes there, not the path to them", name, target, parent, cerr))
	}
}

// checkHandleNoForeignWriters runs CheckNoForeignWriters on a duplicate of
// the open handle h (which keeps its owner), named name in errors.
func checkHandleNoForeignWriters(h windows.Handle, name string) error {
	cur := windows.CurrentProcess()
	var dup windows.Handle
	if err := windows.DuplicateHandle(cur, h, cur, &dup, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return fmt.Errorf("%w: cannot check %s: %w", ErrNotPrivate, name, err)
	}
	f := os.NewFile(uintptr(dup), name)
	defer func() { _ = f.Close() }()
	return CheckNoForeignWriters(f)
}

// walk opens every directory component of rel except the leaf, relative to
// its parent's handle, refusing reparse points. With mkdir, a missing
// component is created with p. It returns the leaf's parent handle and the
// leaf name; done closes the parent handle when the walk opened it.
func (t *Tree) walk(rel string, mkdir bool, p Protection) (parent windows.Handle, leaf string, done func(), err error) {
	comps, err := splitRel(rel)
	if err != nil {
		return 0, "", nil, err
	}
	parent = t.h
	owned := false
	release := func() {
		if owned {
			_ = windows.CloseHandle(parent)
		}
	}
	for i, c := range comps[:len(comps)-1] {
		h, err := t.openDirComp(parent, c)
		if mkdir && errors.Is(err, fs.ErrNotExist) {
			h, err = t.mkdirAt(parent, c, p)
		}
		if err != nil {
			release()
			return 0, "", nil, t.walkErr(strings.Join(comps[:i+1], "/"), err)
		}
		release()
		parent, owned = h, true
	}
	return parent, comps[len(comps)-1], release, nil
}

func (t *Tree) walkErr(rel string, err error) error {
	if errors.Is(err, ErrReparsePoint) {
		return t.reparseErr(rel)
	}
	return &fs.PathError{Op: "open", Path: t.display(rel), Err: err}
}

// mkdirAt creates the directory name relative to parent with p. If it was
// created concurrently it is opened instead, still refusing a reparse
// point.
func (t *Tree) mkdirAt(parent windows.Handle, name string, p Protection) (windows.Handle, error) {
	if err := t.checkACLs(p); err != nil {
		return windows.InvalidHandle, err
	}
	var sd *windows.SECURITY_DESCRIPTOR
	if p == Private {
		id, err := getIdentity()
		if err != nil {
			return windows.InvalidHandle, err
		}
		sd = id.dirSD
	}
	h, _, err := ntCreate(parent, name, dirAccess, 0, shareAll, windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT, sd)
	if errors.Is(err, fs.ErrExist) {
		return t.openDirComp(parent, name)
	}
	return h, err
}

// MkdirAll creates rel and any missing parents with p, each relative to its
// parent's handle. Existing directories are walked (a reparse point is
// refused) and left as they are: an existing directory is never silently
// re-secured.
func (t *Tree) MkdirAll(rel string, p Protection) error {
	parent, leaf, done, err := t.walk(rel, true, p)
	if err != nil {
		return err
	}
	defer done()
	h, err := t.openDirComp(parent, leaf)
	if errors.Is(err, fs.ErrNotExist) {
		h, err = t.mkdirAt(parent, leaf, p)
	}
	if err != nil {
		return t.walkErr(rel, err)
	}
	return windows.CloseHandle(h)
}

// MkdirAllEach is MkdirAll for several directories, with no side effect
// on refusal: it first walks every rel WITHOUT creating anything, opening
// each existing component exactly as MkdirAll would (a reparse point is
// refused; in a FollowLinks tree every existing component must pass
// CheckNoForeignWriters), and only when all of them pass does it create
// the missing ones. So a refused location is found before any directory
// is created anywhere, and a later run finds nothing the tool created
// while deciding not to trust it.
func (t *Tree) MkdirAllEach(p Protection, rels ...string) error {
	for _, rel := range rels {
		if err := t.verifyExisting(rel); err != nil {
			return err
		}
	}
	for _, rel := range rels {
		if err := t.MkdirAll(rel, p); err != nil {
			return err
		}
	}
	return nil
}

// verifyExisting opens the existing components of rel, in order, as the
// walk does, and stops without error at the first missing one. It creates
// nothing.
func (t *Tree) verifyExisting(rel string) error {
	parent, leaf, done, err := t.walk(rel, false, Inherit)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer done()
	h, err := t.openDirComp(parent, leaf)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return t.walkErr(rel, err)
	}
	return windows.CloseHandle(h)
}

// OpenDir opens the existing directory rel through the walk (no reparse
// point at any component), so its privacy can be checked and its entries
// read on the handle.
func (t *Tree) OpenDir(rel string) (*os.File, error) {
	parent, leaf, done, err := t.walk(rel, false, Inherit)
	if err != nil {
		return nil, err
	}
	defer done()
	h, err := t.openDirComp(parent, leaf)
	if err != nil {
		return nil, t.walkErr(rel, err)
	}
	return os.NewFile(uintptr(h), t.display(rel)), nil
}

// openLeaf opens the leaf of rel relative to its parent's handle with
// FILE_OPEN_REPARSE_POINT and refuses it if it is a reparse point.
func (t *Tree) openLeaf(rel string, access, disposition, options uint32, sd *windows.SECURITY_DESCRIPTOR) (windows.Handle, uintptr, error) {
	parent, leaf, done, err := t.walk(rel, false, Inherit)
	if err != nil {
		return windows.InvalidHandle, 0, err
	}
	defer done()
	h, info, err := ntCreate(parent, leaf, access, windows.FILE_ATTRIBUTE_NORMAL, shareAll, disposition,
		options|windows.FILE_OPEN_REPARSE_POINT, sd)
	if err != nil {
		return windows.InvalidHandle, 0, t.walkErr(rel, err)
	}
	if info != fileCreated {
		rp, _, err := isReparse(h)
		if err == nil && rp {
			err = t.reparseErr(rel)
		}
		if err != nil {
			_ = windows.CloseHandle(h)
			return windows.InvalidHandle, 0, err
		}
	}
	return h, info, nil
}

// Create opens rel for reading and writing, creating it with p if it does
// not exist (excl: it must not exist). A new Private file gets the private
// descriptor in the create call. An existing file opened for a Private
// create is verified and repaired or refused as OpenPrivate does. The
// handle shares read, write and delete, so the file can be renamed or
// removed by handle while it is open.
func (t *Tree) Create(rel string, excl bool, p Protection) (*os.File, error) {
	if err := t.checkACLs(p); err != nil {
		return nil, err
	}
	var sd *windows.SECURITY_DESCRIPTOR
	if p == Private {
		id, err := getIdentity()
		if err != nil {
			return nil, err
		}
		sd = id.fileSD
	}
	access := uint32(windows.GENERIC_READ | windows.GENERIC_WRITE | windows.DELETE | windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES)
	disposition := uint32(windows.FILE_CREATE)
	if !excl {
		disposition = windows.FILE_OPEN_IF
		if p == Private {
			access |= windows.WRITE_DAC
		}
	}
	var f *os.File
	err := retryReplaced(t.display(rel), func() error {
		h, info, err := t.openLeaf(rel, access, disposition, windows.FILE_NON_DIRECTORY_FILE, sd)
		if err != nil {
			return err
		}
		f = os.NewFile(uintptr(h), t.display(rel))
		if info == fileOpened {
			afterLinkCheckOpen(f.Name())
			// An existing file opened for writing: one name only (the
			// one-name rule, checked by verifyOrRepair for Private), and
			// for Private the verify-or-repair rule. A file whose name
			// went after the open (errNoNames) is opened again.
			var err error
			if p == Private {
				err = verifyOrRepair(f, true)
			} else {
				err = checkSingleLink(f)
			}
			if err != nil {
				_ = f.Close()
				f = nil
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}

// OpenPrivate opens the existing regular file rel through the walk and
// verifies it with CheckPrivate on the opened handle. Opened for reading
// only, a file that is not private is refused. Opened for writing, a file
// owned by the current user is repaired (its DACL replaced by the private
// one, on the same handle, before any byte is written) and one owned by
// someone else is refused. flag is os.O_RDONLY, os.O_WRONLY or os.O_RDWR,
// optionally with os.O_TRUNC (applied after the check).
func (t *Tree) OpenPrivate(rel string, flag int) (*os.File, error) {
	if flag&^(os.O_RDONLY|os.O_WRONLY|os.O_RDWR|os.O_TRUNC) != 0 {
		return nil, &fs.PathError{Op: "open private", Path: t.display(rel), Err: syscall.EINVAL}
	}
	access := uint32(windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES)
	write := false
	switch flag & (os.O_RDONLY | os.O_WRONLY | os.O_RDWR) {
	case os.O_RDONLY:
		access |= windows.GENERIC_READ
	case os.O_WRONLY:
		access |= windows.GENERIC_WRITE | windows.WRITE_DAC
		write = true
	case os.O_RDWR:
		access |= windows.GENERIC_READ | windows.GENERIC_WRITE | windows.WRITE_DAC
		write = true
	default:
		// O_WRONLY|O_RDWR: refused on both OSes (Linux open(2) would
		// accept it).
		return nil, &fs.PathError{Op: "open private", Path: t.display(rel), Err: syscall.EINVAL}
	}
	var f *os.File
	err := retryReplaced(t.display(rel), func() error {
		h, _, err := t.openLeaf(rel, access, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE, nil)
		if err != nil {
			return err
		}
		f = os.NewFile(uintptr(h), t.display(rel))
		if write {
			// Only a write open checks the link count (verifyOrRepair).
			afterLinkCheckOpen(f.Name())
		}
		// A file opened for writing whose name went after the open
		// (errNoNames) is opened again.
		if err := verifyOrRepair(f, write); err != nil {
			_ = f.Close()
			f = nil
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if write && flag&os.O_TRUNC != 0 {
		if err := f.Truncate(0); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return f, nil
}

// verifyOrRepair applies the verify-or-repair rules on an open handle: pass if
// CheckPrivate passes; otherwise refuse, unless the handle is opened for
// writing and the file is owned by the current user, in which case the
// private DACL is set on this same handle (protected, so inherited ACEs
// are dropped) before any byte is written. The owner is not rewritten: it
// was just verified to be the current user.
//
// A file opened for writing must also have exactly one name (the one-name
// rule):
// a hard-linked file is refused, never repaired.
func verifyOrRepair(f *os.File, write bool) error {
	if write {
		if err := checkSingleLink(f); err != nil {
			return err
		}
	}
	err := CheckPrivate(f)
	if err == nil || !write {
		return err
	}
	owned, oerr := OwnedByCurrentUser(f)
	if oerr != nil {
		return oerr
	}
	if !owned {
		return err
	}
	id, ierr := getIdentity()
	if ierr != nil {
		return ierr
	}
	serr := windows.SetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, id.fileACL, nil)
	runtime.KeepAlive(f)
	if serr != nil {
		return fmt.Errorf("repair the ACL of %s: %w (refusing: %v)", f.Name(), serr, err)
	}
	return CheckPrivate(f)
}

// ReadPrivate reads the regular file rel through the walk and refuses it
// unless CheckPrivate accepts the opened handle. At most max bytes are read
// (no limit when max <= 0).
func (t *Tree) ReadPrivate(rel string, max int64) ([]byte, error) {
	f, err := t.OpenPrivate(rel, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if max <= 0 {
		return io.ReadAll(f)
	}
	return io.ReadAll(io.LimitReader(f, max))
}

// WriteFile replaces rel atomically: a temporary file created next to it
// with p (the private descriptor for Private) is written, synced and
// renamed over rel through its still-open handle. The result has the
// temporary file's protection whatever rel's DACL was, so a too-open file
// is repaired. On failure the temporary file is deleted through the same
// handle; nothing acts on its name.
func (t *Tree) WriteFile(rel string, data []byte, p Protection) error {
	comps, err := splitRel(rel)
	if err != nil {
		return err
	}
	dir := strings.Join(comps[:len(comps)-1], "/")
	f, _, err := t.CreateTemp(dir, "."+comps[len(comps)-1]+".*.tmp", p)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = deleteHandle(windows.Handle(f.Fd()))
			_ = f.Close()
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	// Rename through the handle still open on the file just written, so
	// nothing swapped in under the temporary name can land at rel. This
	// tree created f with protection p, so RenameOpen's privacy check
	// (which would refuse an Inherit file) is not needed here.
	if err := t.renameOpen(f, rel); err != nil {
		return err
	}
	ok = true
	return f.Close()
}

// CreateTemp creates a new file in the directory relDir ("" for the
// anchor) with p, named by pattern as os.CreateTemp does (the last "*" is
// replaced by a random string, or one is appended), and returns it with
// its path relative to the anchor.
func (t *Tree) CreateTemp(relDir, pattern string, p Protection) (*os.File, string, error) {
	if strings.ContainsAny(pattern, `/\`) {
		return nil, "", &fs.PathError{Op: "createtemp", Path: pattern, Err: errors.New("pattern contains a path separator")}
	}
	prefix, suffix := pattern, ""
	if i := strings.LastIndexByte(pattern, '*'); i >= 0 {
		prefix, suffix = pattern[:i], pattern[i+1:]
	}
	for try := 0; ; try++ {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, "", err
		}
		name := prefix + hex.EncodeToString(b[:]) + suffix
		if relDir != "" {
			name = relDir + "/" + name
		}
		f, err := t.Create(name, true, p)
		if err == nil {
			return f, name, nil
		}
		if !errors.Is(err, fs.ErrExist) || try >= 100 {
			return nil, "", err
		}
	}
}

// fileRenameInformationEx is FILE_RENAME_INFORMATION_EX (and, read with
// Flags as a BOOLEAN, FILE_RENAME_INFORMATION): a ULONG or BOOLEAN padded
// to the handle's alignment, the target directory handle, the name length
// in bytes and the name.
type fileRenameInformationEx struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [windows.MAX_PATH]uint16
}

const fileRenameInformationExClass = 65

// Rename renames oldRel to newRel by handle: the source is opened relative
// to its walked parent (a reparse point is refused), and renamed with
// FILE_RENAME_INFORMATION whose RootDirectory is newRel's walked parent, so
// no path is resolved at any point. An existing target is replaced (its
// name only: a reparse point at newRel is replaced, never followed).
//
// To replace a file with one you just wrote, use RenameOpen on the still
// open handle instead: Rename finds oldRel again by name.
func (t *Tree) Rename(oldRel, newRel string) error {
	h, _, err := t.openLeaf(oldRel, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN, 0, nil)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := t.renameHandle(h, newRel); err != nil {
		return &fs.PathError{Op: "rename", Path: t.display(oldRel) + " -> " + t.display(newRel), Err: err}
	}
	return nil
}

// RenameOpen renames the private file open as f to newRel, through that
// same handle (it needs DELETE access, which Create and CreateTemp handles
// carry). The file renamed is the one f was opened on, whatever has
// happened to its name since: a file swapped in under the temporary name is
// never what lands at newRel. f stays open.
//
// What is enforced: f must pass CheckPrivate (verified on the handle before
// the rename), otherwise it is refused with ErrNotPrivate and nothing is
// renamed. f need not come from this tree.
func (t *Tree) RenameOpen(f *os.File, newRel string) error {
	if err := CheckPrivate(f); err != nil {
		return &fs.PathError{Op: "rename", Path: f.Name() + " -> " + t.display(newRel), Err: err}
	}
	return t.renameOpen(f, newRel)
}

// renameOpen is RenameOpen without the privacy check, for files this
// package created itself with a known protection.
func (t *Tree) renameOpen(f *os.File, newRel string) error {
	err := t.renameHandle(windows.Handle(f.Fd()), newRel)
	// f.Fd() is valid only while f is reachable.
	runtime.KeepAlive(f)
	if err != nil {
		return &fs.PathError{Op: "rename", Path: f.Name() + " -> " + t.display(newRel), Err: err}
	}
	return nil
}

// RemoveOpen deletes the file open as f (created by this tree, so its
// handle carries DELETE access) through that same handle, with POSIX
// semantics where supported: the name is removed whatever has happened to
// it, and a file swapped in under that name is left alone. f stays open.
func (t *Tree) RemoveOpen(f *os.File) error {
	err := deleteHandle(windows.Handle(f.Fd()))
	runtime.KeepAlive(f)
	if err != nil {
		return &fs.PathError{Op: "remove", Path: f.Name(), Err: err}
	}
	return nil
}

// renameHandle renames the file open as h to newRel with
// FILE_RENAME_INFORMATION(_EX) whose RootDirectory is newRel's walked
// parent (a reparse point on the way is refused).
func (t *Tree) renameHandle(h windows.Handle, newRel string) error {
	newParent, newLeaf, done, err := t.walk(newRel, false, Inherit)
	if err != nil {
		return err
	}
	defer done()
	name, err := windows.UTF16FromString(newLeaf)
	if err != nil {
		return err
	}
	var info fileRenameInformationEx
	if len(name) > len(info.FileName) {
		return syscall.ENAMETOOLONG
	}
	copy(info.FileName[:], name)
	info.FileNameLength = uint32((len(name) - 1) * 2)
	info.RootDirectory = newParent
	// POSIX semantics replace a target that is open elsewhere (a reader).
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	err = windows.NtSetInformationFile(h, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)), fileRenameInformationExClass)
	if err == windows.STATUS_INVALID_INFO_CLASS || err == windows.STATUS_INVALID_PARAMETER || err == windows.STATUS_NOT_SUPPORTED {
		// The OS or file system lacks FILE_RENAME_INFORMATION_EX or POSIX
		// semantics (FAT): retry with FILE_RENAME_INFORMATION,
		// ReplaceIfExists = TRUE. Any other failure is reported as is.
		info.Flags = 1
		err = windows.NtSetInformationFile(h, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&info)),
			uint32(unsafe.Sizeof(info)), windows.FileRenameInformation)
	}
	if err != nil {
		return ntErr(err)
	}
	return nil
}

type fileDispositionInformationEx struct {
	Flags uint32
}

type fileDispositionInformation struct {
	DeleteFile bool
}

// Remove removes rel by handle: it is opened relative to its walked parent
// with DELETE and FILE_OPEN_REPARSE_POINT (a reparse point is refused) and
// marked for deletion, with POSIX semantics where the file system supports
// them.
func (t *Tree) Remove(rel string) error {
	h, _, err := t.openLeaf(rel, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN, 0, nil)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := deleteHandle(h); err != nil {
		return &fs.PathError{Op: "remove", Path: t.display(rel), Err: err}
	}
	return nil
}

// deleteHandle marks the file open as h (with DELETE access) for deletion:
// POSIX semantics (the name goes at once) where supported, else classic
// delete-on-close, which is equivalent for these files.
func deleteHandle(h windows.Handle) error {
	ex := fileDispositionInformationEx{Flags: windows.FILE_DISPOSITION_DELETE |
		windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE}
	err := windows.NtSetInformationFile(h, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&ex)),
		uint32(unsafe.Sizeof(ex)), windows.FileDispositionInformationEx)
	switch err {
	case nil:
		return nil
	case windows.STATUS_INVALID_INFO_CLASS, windows.STATUS_INVALID_PARAMETER, windows.STATUS_NOT_SUPPORTED:
		// No POSIX delete here (older Windows, FAT).
		d := fileDispositionInformation{DeleteFile: true}
		err = windows.NtSetInformationFile(h, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&d)),
			uint32(unsafe.Sizeof(d)), windows.FileDispositionInformation)
	}
	if err != nil {
		return ntErr(err)
	}
	return nil
}

// securityOf reads the owner and DACL of the open handle.
func securityOf(f *os.File) (owner *windows.SID, dacl *windows.ACL, err error) {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	runtime.KeepAlive(f) // f.Fd() is valid only while f is reachable
	if err != nil {
		// A descriptor that cannot be read cannot be verified: refuse.
		return nil, nil, fmt.Errorf("%w: cannot read the security descriptor of %s: %w", ErrNotPrivate, f.Name(), err)
	}
	owner, _, err = sd.Owner()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: cannot read the owner of %s: %w", ErrNotPrivate, f.Name(), err)
	}
	dacl, _, err = sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
		// No DACL present at all: same as a NULL DACL, everyone has access.
		return owner, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read the DACL of %s: %w", f.Name(), err)
	}
	return owner, dacl, nil
}

func (id *identity) allowedSID(sid *windows.SID) bool {
	return sid.Equals(id.user) || sid.Equals(id.system) || sid.Equals(id.admins)
}

// checkACL is the shared predicate of CheckPrivate and
// CheckNoForeignWriters. A non-zero writeMask restricts the foreign-SID
// rule to allow ACEs granting any of those rights; zero means any allow
// ACE for a foreign SID fails. anyAllowedOwner accepts SYSTEM and
// Administrators as owners besides the current user (the integrity-only
// rule); otherwise the owner must be the current user.
func checkACL(f *os.File, writeMask uint32, anyAllowedOwner bool) error {
	id, err := getIdentity()
	if err != nil {
		return err
	}
	owner, dacl, err := securityOf(f)
	if err != nil {
		return err
	}
	if owner == nil {
		// No owner at all (a volume without persistent ACLs, some
		// redirectors). EqualSid on a NULL SID would crash: refuse.
		return fmt.Errorf("%w: %s has no owner in its security descriptor", ErrNotPrivate, f.Name())
	}
	if anyAllowedOwner && !id.allowedSID(owner) {
		return fmt.Errorf("%w: %s is owned by %s, which is not the current user %s, SYSTEM or Administrators", ErrNotPrivate, f.Name(), owner.String(), id.user.String())
	}
	if !anyAllowedOwner && !owner.Equals(id.user) {
		return fmt.Errorf("%w: %s is owned by %s, not the current user %s", ErrNotPrivate, f.Name(), owner.String(), id.user.String())
	}
	if dacl == nil {
		return fmt.Errorf("%w: %s has a NULL DACL (everyone has full access)", ErrNotPrivate, f.Name())
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("read ACE %d of %s: %w", i, f.Name(), err)
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			// Callback, object, audit-in-DACL or unknown ACE types: their
			// effect cannot be judged here, so they fail closed.
			return fmt.Errorf("%w: %s has an ACE of type %d, which is not a plain allow or deny ACE", ErrNotPrivate, f.Name(), ace.Header.AceType)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if id.allowedSID(sid) {
			continue
		}
		if writeMask != 0 && uint32(ace.Mask)&writeMask == 0 {
			continue
		}
		// Inherit-only ACEs are included: on a directory they grant access
		// to everything created inside it.
		return fmt.Errorf("%w: %s grants access to %s (mask %#x); scion recreates the file with an owner-only ACL if you delete it", ErrNotPrivate, f.Name(), sid.String(), uint32(ace.Mask))
	}
	return nil
}

// CheckPrivate verifies the open handle's security descriptor: the owner is
// the current user; the DACL is present (not NULL); every ACE is a plain
// allow or deny ACE; and every allow ACE (inherit-only ones included) is
// for the current user, SYSTEM (S-1-5-18) or Administrators
// (S-1-5-32-544), compared by SID. Administrators are accepted on verify
// (they can take ownership anyway) but never granted on create. The
// handle needs READ_CONTROL access.
func CheckPrivate(f *os.File) error {
	return checkACL(f, 0, false)
}

// fileDeleteChild is FILE_DELETE_CHILD (not defined in x/sys/windows).
const fileDeleteChild = 0x40

// foreignWriteMask is every right that lets a principal change a file or
// directory's content, metadata, ACL or owner.
const foreignWriteMask = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | fileDeleteChild |
	windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES | windows.DELETE | windows.WRITE_DAC |
	windows.WRITE_OWNER | windows.GENERIC_WRITE | windows.GENERIC_ALL | windows.MAXIMUM_ALLOWED

// CheckNoForeignWriters is the integrity-only variant of CheckPrivate: the
// owner is the current user, SYSTEM (S-1-5-18) or Administrators
// (S-1-5-32-544), compared by SID; every ACE is a plain allow or deny ACE;
// and no
// allow ACE grants a write right (FILE_WRITE_DATA/FILE_ADD_FILE,
// FILE_APPEND_DATA/FILE_ADD_SUBDIRECTORY, FILE_DELETE_CHILD, FILE_WRITE_EA,
// FILE_WRITE_ATTRIBUTES, DELETE, WRITE_DAC, WRITE_OWNER, GENERIC_WRITE,
// GENERIC_ALL, MAXIMUM_ALLOWED) to a SID other than the current user,
// SYSTEM or Administrators.
//
// The owner set is consistent with CheckPrivate (Administrators accepted
// on verify, by well-known SID only): an elevated shell makes Administrators the
// default owner of what it creates (ssh-keygen, an editor), admins and
// SYSTEM already own the machine and can take ownership anyway, and
// Win32-OpenSSH itself accepts these owners for its config. Refusing them
// would push users to loosen ACLs on key material. CheckPrivate (the
// confidentiality rule) is unchanged: its owner must be the current user.
// The SIDs are token-derived or well-known (CreateWellKnownSid), never
// looked up by name.
func CheckNoForeignWriters(f *os.File) error {
	return checkACL(f, foreignWriteMask, true)
}

// ownerAllowed reports whether the open handle's owner is the current
// user, SYSTEM or Administrators (the CheckNoForeignWriters owner set).
func ownerAllowed(f *os.File) (bool, error) {
	id, err := getIdentity()
	if err != nil {
		return false, err
	}
	owner, _, err := securityOf(f)
	if err != nil {
		return false, err
	}
	return owner != nil && id.allowedSID(owner), nil
}

// OwnedByCurrentUser reports whether the open handle's owner is the current
// user's SID.
func OwnedByCurrentUser(f *os.File) (bool, error) {
	id, err := getIdentity()
	if err != nil {
		return false, err
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	runtime.KeepAlive(f)
	if err != nil {
		return false, fmt.Errorf("read the owner of %s: %w", f.Name(), err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, fmt.Errorf("read the owner of %s: %w", f.Name(), err)
	}
	if owner == nil {
		return false, nil
	}
	return owner.Equals(id.user), nil
}

// checkSingleLink refuses the open file unless it has exactly one name
// (the one-name rule). NumberOfLinks comes from
// GetFileInformationByHandle on the handle itself. If it cannot be read the
// file is refused (fail closed), as the attribute fallback does for
// anything but ERROR_INVALID_PARAMETER. A count of zero (the name was
// removed after the open) is refused with errNoNames, so the caller can
// open the path again.
func checkSingleLink(f *os.File) error {
	var bhfi windows.ByHandleFileInformation
	err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &bhfi)
	runtime.KeepAlive(f)
	if err != nil {
		return fmt.Errorf("refusing %s: cannot read its link count: %v: %w", f.Name(), err, ErrHardLinked)
	}
	if bhfi.NumberOfLinks == 0 {
		// The name went after the open: another writer renamed a file
		// over it or deleted it with POSIX semantics (the handle shares
		// delete, see shareAll). Not a hard link; the caller opens the
		// path again (retryReplaced).
		return fmt.Errorf("refusing %s: %w", f.Name(), errNoNames)
	}
	if bhfi.NumberOfLinks != 1 {
		return fmt.Errorf("refusing %s: it has %d names: %w", f.Name(), bhfi.NumberOfLinks, ErrHardLinked)
	}
	return nil
}

// anchorFile returns a duplicate of the anchor handle as an *os.File, so
// the handle checks (CheckNoForeignWriters, CheckPrivate) can run on the
// anchor directory itself. The caller closes it.
func (t *Tree) anchorFile() (*os.File, error) {
	cur := windows.CurrentProcess()
	var dup windows.Handle
	if err := windows.DuplicateHandle(cur, t.h, cur, &dup, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, &fs.PathError{Op: "open", Path: t.anchor, Err: err}
	}
	return os.NewFile(uintptr(dup), t.anchor), nil
}
