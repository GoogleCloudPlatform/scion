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

// Package syncarchive extracts a tar archive copied from an agent into a
// directory on the host.
//
// The extractor writes only regular files, directories, and symbolic links
// whose targets stay inside the destination, and it writes all of them
// through an [os.Root] opened on the destination, so no entry can reach
// outside it. Entries under a ".scion" directory (at any depth, compared
// case-insensitively) are skipped, so the copy never changes project
// configuration on the host. Each regular file is first written to a
// temporary name in its directory and then renamed into place, so an
// interrupted stream never leaves a partly written file behind.
//
// The rules, per entry:
//
//   - A name that is absolute or has a ".." component refuses the whole
//     archive (an archive made with "tar -C dir ." never has one).
//   - An entry with a ".scion" component, or under one of
//     [Options.Excludes], is skipped.
//   - A directory, or a regular file, is written. Modes keep the
//     permission bits only (no setuid, setgid or sticky bit) and are
//     subject to the process umask; modification times and ownership are
//     not restored. Directories always keep owner read, write and search
//     permission so their contents can be written.
//   - A symbolic link is written only if its target is relative, any ".."
//     components come before all other components, the target resolves
//     inside the destination, and it names no ".scion" component.
//     Otherwise it is skipped and reported.
//   - A hard link is written only if it names an existing regular file
//     inside the destination that is not skipped. Otherwise it is skipped
//     and reported.
//   - Device nodes, FIFOs and any other entry type are skipped and
//     reported.
//   - An entry whose parent path contains a symbolic link or a non-directory
//     is skipped and reported: the extractor never writes through a link.
//
// The archive is bounded by entry count and by the total size of entry
// contents; exceeding either refuses the rest of the archive.
package syncarchive

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"runtime"
	"strings"
)

const (
	// DefaultMaxEntries is the entry limit used when Options.MaxEntries is 0.
	DefaultMaxEntries = 2_000_000
	// DefaultMaxBytes is the content size limit used when Options.MaxBytes
	// is 0.
	DefaultMaxBytes int64 = 64 << 30

	// maxReported caps the number of skipped entries kept in Result.Unsafe.
	maxReported = 50

	// ProtectedDir is the directory name the extractor never writes under.
	ProtectedDir = ".scion"
)

// unsafeNameChars are characters never accepted in an entry name or link
// target. On Windows a backslash is a separator and a colon names a drive
// or stream, so both are refused there; elsewhere they are ordinary
// characters.
var unsafeNameChars = func() string {
	if runtime.GOOS == "windows" {
		return "\\:\x00"
	}
	return "\x00"
}()

// ErrRefused wraps every error that refuses an archive because of its
// content (as opposed to an I/O failure).
var ErrRefused = errors.New("archive refused")

// Options configures Extract.
type Options struct {
	// Excludes are paths relative to the archive root that are skipped,
	// together with everything below them. They are matched literally.
	Excludes []string
	// MaxEntries limits the number of archive entries; 0 means
	// DefaultMaxEntries.
	MaxEntries int
	// MaxBytes limits the summed content size of all entries; 0 means
	// DefaultMaxBytes.
	MaxBytes int64
}

// Skip records an entry that was not written, with the reason.
type Skip struct {
	Name   string
	Reason string
}

// Result summarises an extraction.
type Result struct {
	Files     int
	Dirs      int
	Symlinks  int
	Hardlinks int
	Bytes     int64
	// Protected counts entries skipped because they are under a ".scion"
	// directory or one of Options.Excludes.
	Protected int
	// UnsafeCount counts entries skipped because writing them would not be
	// safe (link targets outside the destination, device nodes, and so
	// on). Unsafe lists the first of them.
	UnsafeCount int
	Unsafe      []Skip
}

func (r *Result) unsafe(name, reason string) {
	r.UnsafeCount++
	if len(r.Unsafe) < maxReported {
		r.Unsafe = append(r.Unsafe, Skip{Name: name, Reason: reason})
	}
}

// ExtractGzip extracts the gzip-compressed tar stream r into dest; see
// Extract.
func ExtractGzip(r io.Reader, dest string, opts Options) (Result, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return Result{}, fmt.Errorf("read archive: %w", err)
	}
	res, err := Extract(zr, dest, opts)
	if err != nil {
		return res, err
	}
	// Read to the end so a corrupt trailer is reported.
	if _, err := io.Copy(io.Discard, zr); err != nil {
		return res, fmt.Errorf("read archive: %w", err)
	}
	return res, zr.Close()
}

// Extract extracts the tar stream r into the existing directory dest under
// the rules in the package documentation. On error, entries written before
// the failing one stay in place.
func Extract(r io.Reader, dest string, opts Options) (Result, error) {
	var res Result
	root, err := os.OpenRoot(dest)
	if err != nil {
		return res, fmt.Errorf("open destination: %w", err)
	}
	defer root.Close()

	maxEntries := opts.MaxEntries
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	excludes := make([][]string, 0, len(opts.Excludes))
	for _, e := range opts.Excludes {
		if parts := splitClean(e); len(parts) > 0 {
			excludes = append(excludes, parts)
		}
	}

	x := &extractor{root: root, res: &res, excludes: excludes}
	tr := tar.NewReader(r)
	var entries int
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return res, nil
		}
		if err != nil {
			if errors.Is(err, tar.ErrInsecurePath) && hdr != nil {
				return res, fmt.Errorf("%w: entry %q has an unsafe name", ErrRefused, hdr.Name)
			}
			return res, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		entries++
		if entries > maxEntries {
			return res, fmt.Errorf("%w: more than %d entries", ErrRefused, maxEntries)
		}
		if hdr.Size < 0 || hdr.Size > maxBytes-total {
			return res, fmt.Errorf("%w: contents larger than %d bytes", ErrRefused, maxBytes)
		}
		total += hdr.Size
		if err := x.entry(tr, hdr); err != nil {
			return res, err
		}
	}
}

type extractor struct {
	root     *os.Root
	res      *Result
	excludes [][]string
}

// entryName validates an archive member name and returns its components
// relative to the archive root. A name naming the root itself returns no
// components.
func entryName(name string) ([]string, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: entry with an empty name", ErrRefused)
	}
	if strings.HasPrefix(name, "/") || strings.ContainsAny(name, unsafeNameChars) {
		return nil, fmt.Errorf("%w: entry %q has an unsafe name", ErrRefused, name)
	}
	var parts []string
	for _, p := range strings.Split(name, "/") {
		switch p {
		case "", ".":
		case "..":
			return nil, fmt.Errorf("%w: entry %q leaves the destination", ErrRefused, name)
		default:
			parts = append(parts, p)
		}
	}
	return parts, nil
}

// splitClean splits a slash-separated relative path into its components,
// dropping empty and "." components.
func splitClean(p string) []string {
	var parts []string
	for _, c := range strings.Split(path.Clean("/"+p), "/") {
		if c != "" {
			parts = append(parts, c)
		}
	}
	return parts
}

// IsProtectedName reports whether a path component names the protected
// directory on any file system the host might use: compared with Unicode
// case folding, ignoring trailing dots and spaces (Windows) and code points
// some file systems ignore in names (HFS+).
func IsProtectedName(c string) bool {
	var b strings.Builder
	for _, r := range c {
		if ignorableRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.EqualFold(strings.TrimRight(b.String(), ". "), ProtectedDir)
}

func ignorableRune(r rune) bool {
	switch {
	case r == 0x00AD, r == 0x034F, r == 0xFEFF:
		return true
	case r >= 0x200B && r <= 0x200F:
		return true
	case r >= 0x202A && r <= 0x202E:
		return true
	case r >= 0x2060 && r <= 0x206F:
		return true
	case r >= 0xFE00 && r <= 0xFE0F:
		return true
	}
	return false
}

func hasProtected(parts []string) bool {
	for _, p := range parts {
		if IsProtectedName(p) {
			return true
		}
	}
	return false
}

func (x *extractor) excluded(parts []string) bool {
	for _, e := range x.excludes {
		if len(parts) >= len(e) && equalParts(parts[:len(e)], e) {
			return true
		}
	}
	return false
}

func equalParts(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (x *extractor) entry(tr *tar.Reader, hdr *tar.Header) error {
	parts, err := entryName(hdr.Name)
	if err != nil {
		return err
	}
	if len(parts) == 0 {
		// The archive root: the destination already exists, and its own
		// mode is left alone.
		return nil
	}
	if hasProtected(parts) || x.excluded(parts) {
		x.res.Protected++
		return nil
	}
	rel := strings.Join(parts, "/")

	switch hdr.Typeflag {
	case tar.TypeDir:
		return x.dir(parts, rel, hdr)
	case tar.TypeReg, tar.TypeRegA, tar.TypeGNUSparse:
		return x.file(tr, parts, rel, hdr)
	case tar.TypeSymlink:
		return x.symlink(parts, rel, hdr)
	case tar.TypeLink:
		return x.hardlink(parts, rel, hdr)
	case tar.TypeChar, tar.TypeBlock:
		x.res.unsafe(rel, "device node")
	case tar.TypeFifo:
		x.res.unsafe(rel, "FIFO")
	default:
		x.res.unsafe(rel, fmt.Sprintf("unsupported entry type %q", hdr.Typeflag))
	}
	return nil
}

// dirMode is the mode a directory from the archive is created with.
func dirMode(hdr *tar.Header) fs.FileMode {
	return fs.FileMode(hdr.Mode)&fs.ModePerm | 0o700
}

// parents makes sure every parent directory of parts exists as a real
// directory, creating missing ones. It returns false (and records the
// entry as skipped) when a parent is a symbolic link or not a directory.
func (x *extractor) parents(parts []string, rel string) (bool, error) {
	for i := 1; i < len(parts); i++ {
		p := strings.Join(parts[:i], "/")
		fi, err := x.root.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := x.root.Mkdir(p, 0o755); err != nil {
				return false, fmt.Errorf("create %s: %w", p, err)
			}
		case err != nil:
			return false, fmt.Errorf("inspect %s: %w", p, err)
		case fi.Mode()&fs.ModeSymlink != 0:
			x.res.unsafe(rel, "parent "+p+" is a symbolic link")
			return false, nil
		case !fi.IsDir():
			x.res.unsafe(rel, "parent "+p+" is not a directory")
			return false, nil
		}
	}
	return true, nil
}

func (x *extractor) dir(parts []string, rel string, hdr *tar.Header) error {
	ok, err := x.parents(parts, rel)
	if err != nil || !ok {
		return err
	}
	fi, err := x.root.Lstat(rel)
	switch {
	case err == nil && fi.IsDir():
		x.res.Dirs++
		return nil
	case err == nil:
		// A file or link where the archive has a directory is replaced,
		// as tar does; a link is removed, never followed.
		if err := x.root.Remove(rel); err != nil {
			return fmt.Errorf("replace %s: %w", rel, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("inspect %s: %w", rel, err)
	}
	if err := x.root.Mkdir(rel, dirMode(hdr)); err != nil {
		return fmt.Errorf("create %s: %w", rel, err)
	}
	x.res.Dirs++
	return nil
}

// tempName returns an unused-looking name in the directory of rel.
func tempName(rel string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	name := ".sync-extract-" + hex.EncodeToString(b[:])
	if dir := path.Dir(rel); dir != "." {
		name = dir + "/" + name
	}
	return name, nil
}

// place renames the staged entry tmp to rel, replacing a file or link
// there (a link is replaced, not followed). A directory at rel is not
// replaced: the rename fails and the error is returned.
func (x *extractor) place(tmp, rel string) error {
	if err := x.root.Rename(tmp, rel); err != nil {
		_ = x.root.Remove(tmp)
		return fmt.Errorf("write %s: %w", rel, err)
	}
	return nil
}

func (x *extractor) file(tr *tar.Reader, parts []string, rel string, hdr *tar.Header) error {
	ok, err := x.parents(parts, rel)
	if err != nil || !ok {
		return err
	}
	tmp, err := tempName(rel)
	if err != nil {
		return err
	}
	f, err := x.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fs.FileMode(hdr.Mode)&fs.ModePerm)
	if err != nil {
		return fmt.Errorf("create %s: %w", rel, err)
	}
	n, err := io.Copy(f, tr)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = x.root.Remove(tmp)
		return fmt.Errorf("write %s: %w", rel, err)
	}
	if err := x.place(tmp, rel); err != nil {
		return err
	}
	x.res.Files++
	x.res.Bytes += n
	return nil
}

// symlinkTargetOK reports whether a symbolic link at rel (components
// parts) pointing at target stays inside the destination without naming
// the protected directory. The link's own directory is a real directory
// (parents checks that), so leading ".." components climb exactly as far
// as they say; ".." after any other component could climb out of a
// directory reached through another link, so it is not allowed.
func symlinkTargetOK(parts []string, target string) string {
	if target == "" || strings.HasPrefix(target, "/") || strings.ContainsAny(target, unsafeNameChars) {
		return "link target is absolute or unsafe"
	}
	depth := len(parts) - 1
	descended := false
	for _, c := range strings.Split(target, "/") {
		switch c {
		case "", ".":
		case "..":
			if descended {
				return "link target has '..' after another component"
			}
			depth--
			if depth < 0 {
				return "link target leaves the destination"
			}
		default:
			if IsProtectedName(c) {
				return "link target names " + ProtectedDir
			}
			descended = true
		}
	}
	// The resolved path must not name the protected directory either (for
	// example a link in a subdirectory climbing to the root's ".scion").
	resolved := splitClean(path.Join(path.Dir(strings.Join(parts, "/")), target))
	if hasProtected(resolved) {
		return "link target names " + ProtectedDir
	}
	return ""
}

func (x *extractor) symlink(parts []string, rel string, hdr *tar.Header) error {
	if reason := symlinkTargetOK(parts, hdr.Linkname); reason != "" {
		x.res.unsafe(rel, reason)
		return nil
	}
	ok, err := x.parents(parts, rel)
	if err != nil || !ok {
		return err
	}
	tmp, err := tempName(rel)
	if err != nil {
		return err
	}
	if err := x.root.Symlink(hdr.Linkname, tmp); err != nil {
		return fmt.Errorf("create %s: %w", rel, err)
	}
	if err := x.place(tmp, rel); err != nil {
		return err
	}
	x.res.Symlinks++
	return nil
}

func (x *extractor) hardlink(parts []string, rel string, hdr *tar.Header) error {
	target, err := entryName(hdr.Linkname)
	if err != nil || len(target) == 0 {
		x.res.unsafe(rel, "link target is unsafe")
		return nil
	}
	if hasProtected(target) || x.excluded(target) {
		x.res.unsafe(rel, "link target is skipped")
		return nil
	}
	// The target must be a regular file reached through real directories.
	if ok, err := x.realDirs(target); err != nil || !ok {
		if err == nil {
			x.res.unsafe(rel, "link target is not a regular file")
		}
		return err
	}
	targetRel := strings.Join(target, "/")
	fi, err := x.root.Lstat(targetRel)
	if err != nil || !fi.Mode().IsRegular() {
		x.res.unsafe(rel, "link target is not a regular file")
		return nil
	}
	ok, err := x.parents(parts, rel)
	if err != nil || !ok {
		return err
	}
	tmp, err := tempName(rel)
	if err != nil {
		return err
	}
	if err := x.root.Link(targetRel, tmp); err != nil {
		return fmt.Errorf("create %s: %w", rel, err)
	}
	if err := x.place(tmp, rel); err != nil {
		return err
	}
	x.res.Hardlinks++
	return nil
}

// realDirs reports whether every parent directory of parts exists as a
// real directory (not a link), without creating anything.
func (x *extractor) realDirs(parts []string) (bool, error) {
	for i := 1; i < len(parts); i++ {
		fi, err := x.root.Lstat(strings.Join(parts[:i], "/"))
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !fi.IsDir() {
			return false, nil
		}
	}
	return true, nil
}
