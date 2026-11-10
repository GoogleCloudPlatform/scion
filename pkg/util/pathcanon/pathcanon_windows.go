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

package pathcanon

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Resolve returns the canonical form of p.
//
// p is made absolute with filepath.Abs (GetFullPathNameW: separators
// normalised to '\', "." and ".." collapsed, trailing dots and spaces
// stripped the way the file system would strip them). Every component
// below the volume must then pass ValidName, so an alternate data stream
// or a device name can never reach an open. The deepest existing ancestor
// is opened, following links, and its name is taken from
// GetFinalPathNameByHandle(FILE_NAME_NORMALIZED|VOLUME_NAME_DOS), which
// resolves 8.3 short names, case, junctions and symlinks. The
// not-yet-existing tail is appended; an entry that exists but cannot be
// followed (a dangling symlink or junction) is ErrDanglingLink, never tail. The output keeps the \\?\ prefix, so
// two Resolve outputs always compare like with like.
func Resolve(p string) (string, error) {
	if p == "" {
		return "", invalid(p, "empty path")
	}
	if strings.ContainsRune(p, 0) {
		return "", invalid(p, "contains a NUL byte")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	vol := filepath.VolumeName(abs)
	if isDeviceNamespace(vol) {
		return "", invalid(p, "names a device, not a file")
	}
	for _, c := range strings.Split(strings.Trim(abs[len(vol):], `\`), `\`) {
		if c == "" {
			continue
		}
		if err := ValidName(c); err != nil {
			return "", err
		}
	}

	cur := abs
	var tail []string
	for {
		final, isDir, err := finalPath(cur)
		if err == nil {
			if len(tail) > 0 && !isDir {
				// Nothing can exist below a file: refuse, as unix does.
				return "", &fs.PathError{Op: "resolve", Path: cur, Err: syscall.ENOTDIR}
			}
			for i := len(tail) - 1; i >= 0; i-- {
				final = strings.TrimRight(final, `\`) + `\` + tail[i]
			}
			return final, nil
		}
		if !notFound(err) {
			return "", err
		}
		// The following open found nothing. Probe cur itself without
		// following it: if that succeeds, cur exists and is a link whose
		// target is missing, which must never become tail.
		if perr := probeNoFollow(cur); perr == nil {
			return "", fmt.Errorf("resolve %s: %w", cur, ErrDanglingLink)
		} else if !notFound(perr) {
			return "", perr
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// notFound reports an open error meaning nothing is at the path. Besides
// file/path not found, that includes ERROR_DIRECTORY, which some Windows
// versions return when an intermediate component is a file; the loop then
// reaches that file and refuses with ENOTDIR.
func notFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, windows.ERROR_DIRECTORY)
}

// probeNoFollow opens path itself, not following a reparse point at its
// last component, and closes it again. A nil result means the entry
// exists.
func probeNoFollow(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return windows.CloseHandle(h)
}

// isDeviceNamespace reports whether a volume name is in the Win32 device
// namespace (\\.\ or \??\), which GetFullPathNameW produces for a device
// name such as "C:\dir\NUL" on some Windows versions.
func isDeviceNamespace(vol string) bool {
	return strings.HasPrefix(vol, `\\.\`) || strings.HasPrefix(vol, `\??\`)
}

// GetFinalPathNameByHandle flags (not defined in x/sys/windows).
const (
	fileNameNormalized = 0x0
	volumeNameDOS      = 0x0
)

// finalPath opens path, following links, and returns its normalised DOS
// name (with the \\?\ prefix) and whether it is a directory.
func finalPath(path string) (string, bool, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", false, err
	}
	h, err := windows.CreateFile(p, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", false, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	defer func() { _ = windows.CloseHandle(h) }()
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)),
			fileNameNormalized|volumeNameDOS)
		if err != nil {
			return "", false, &fs.PathError{Op: "GetFinalPathNameByHandle", Path: path, Err: err}
		}
		if int(n) < len(buf) {
			var bhfi windows.ByHandleFileInformation
			if err := windows.GetFileInformationByHandle(h, &bhfi); err != nil {
				return "", false, &fs.PathError{Op: "GetFileInformationByHandle", Path: path, Err: err}
			}
			return windows.UTF16ToString(buf[:n]), bhfi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0, nil
		}
		buf = make([]uint16, n+1)
	}
}

// reservedDeviceNames are the names Windows maps to a device in any
// directory, with or without an extension.
var reservedDeviceNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"CONIN$": true, "CONOUT$": true,
	"COM0": true, "COM1": true, "COM2": true, "COM3": true, "COM4": true,
	"COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"COM¹": true, "COM²": true, "COM³": true,
	"LPT0": true, "LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true,
	"LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
	"LPT¹": true, "LPT²": true, "LPT³": true,
}

// ValidName accepts a name that becomes exactly one path component. It
// rejects: the empty name, "." and ".."; '/' and '\'; ':' (an alternate
// data stream); control characters and the characters Windows never
// allows in a file name (< > " | ? *); a trailing '.' or ' ' (the file
// system strips it, so the name would alias another); and the reserved
// device names (CON, PRN, AUX, NUL, CONIN$, CONOUT$, COM0-9, LPT0-9 and
// the superscript COM¹²³/LPT¹²³), also with any extension.
func ValidName(name string) error {
	switch {
	case name == "":
		return invalid(name, "empty name")
	case name == "." || name == "..":
		return invalid(name, "not a file name")
	case !utf8.ValidString(name):
		return invalid(name, "not valid UTF-8")
	}
	for _, r := range name {
		switch {
		case r == '/' || r == '\\':
			return invalid(name, "contains a path separator")
		case r == ':':
			return invalid(name, "contains ':' (alternate data stream)")
		case r < 0x20 || r == 0x7f:
			return invalid(name, "contains a control character")
		case strings.ContainsRune(`<>"|?*`, r):
			return invalid(name, "contains a character Windows does not allow in a file name")
		}
	}
	if last := name[len(name)-1]; last == '.' || last == ' ' {
		return invalid(name, "ends in a dot or a space")
	}
	base := name
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if reservedDeviceNames[strings.ToUpper(strings.TrimRight(base, " "))] {
		return invalid(name, "is a reserved device name")
	}
	return nil
}

// ValidInputPath accepts a caller-supplied whole path. It rejects the
// empty path, the \\?\, \\.\ and \??\ prefixes, UNC paths (\\server\share,
// in either separator), and any component ValidName rejects. "." and ".."
// components and a leading drive ("C:") are allowed.
//
// This is the only place those prefixes are rejected: containment compares
// Resolve output with Resolve output, and both carry \\?\.
func ValidInputPath(p string) error {
	if p == "" {
		return invalid(p, "empty path")
	}
	isSep := func(c byte) bool { return c == '\\' || c == '/' }
	if len(p) >= 2 && isSep(p[0]) && isSep(p[1]) {
		return invalid(p, `extended-length (\\?\), device (\\.\) and UNC (\\server\share) paths are not accepted`)
	}
	if len(p) >= 4 && isSep(p[0]) && p[1] == '?' && p[2] == '?' && isSep(p[3]) {
		return invalid(p, `NT object paths (\??\) are not accepted`)
	}
	rest := p
	if len(rest) >= 2 && rest[1] == ':' && isASCIILetter(rest[0]) {
		rest = rest[2:]
	}
	for _, c := range strings.FieldsFunc(rest, func(r rune) bool { return r == '\\' || r == '/' }) {
		if c == "." || c == ".." {
			continue
		}
		if err := ValidName(c); err != nil {
			return err
		}
	}
	return nil
}

func isASCIILetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

// within compares two Resolve outputs per component, case-insensitively.
//
// Components are compared with CompareStringOrdinal(ignoreCase=TRUE), which
// uses the operating system's uppercase table: the same rule NTFS applies
// to names. strings.EqualFold (Unicode simple folding) is not that rule:
// it equates "K" and the Kelvin sign U+212A, which NTFS keeps distinct, so
// it would judge a distinct sibling directory to be inside the root.
func within(r, q string) bool {
	rc := strings.Split(strings.TrimRight(r, `\`), `\`)
	qc := strings.Split(strings.TrimRight(q, `\`), `\`)
	if len(qc) < len(rc) {
		return false
	}
	for i := range rc {
		if !equalNames(rc[i], qc[i]) {
			return false
		}
	}
	return true
}

var procCompareStringOrdinal = windows.NewLazySystemDLL("kernel32.dll").NewProc("CompareStringOrdinal")

const cstrEqual = 2 // CSTR_EQUAL

// equalNames compares two path components the way the file system does:
// ordinal, case-insensitive through the OS uppercase table. Any failure
// compares unequal, which fails closed (not Within).
func equalNames(a, b string) bool {
	if a == b {
		return true
	}
	ua, err := windows.UTF16FromString(a)
	if err != nil {
		return false
	}
	ub, err := windows.UTF16FromString(b)
	if err != nil {
		return false
	}
	if len(ua) <= 1 || len(ub) <= 1 {
		return false
	}
	r, _, _ := procCompareStringOrdinal.Call(
		uintptr(unsafe.Pointer(&ua[0])), uintptr(len(ua)-1),
		uintptr(unsafe.Pointer(&ub[0])), uintptr(len(ub)-1),
		1)
	return r == cstrEqual
}
