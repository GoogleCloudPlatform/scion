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

package bundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

// ExtractFromManifest validates m (refusing unknown manifest versions) and
// extracts the named root ("workspace" or "home") into staging.
//
// On any error, staging may contain partially extracted files or an escaping
// symlink detected during the post-pass; the caller must discard the staging
// directory and never commit it.
func ExtractFromManifest(ctx context.Context, staging *os.Root, scratchDir string, m Manifest, rootName string, lim Limits, src PartSource) error {
	if err := m.Validate(); err != nil {
		return err
	}
	rm, ok := m.Roots[rootName]
	if !ok {
		return fmt.Errorf("%w: root %q not found in manifest", ErrInvalidManifest, rootName)
	}
	return Extract(ctx, staging, scratchDir, rm, lim, src)
}

// Extract verifies and extracts all parts described by rm into staging.
//
// Each part is downloaded into a temporary file in scratchDir (never inside
// staging; empty defaults to os.TempDir) and its exact byte length and SHA-256
// digest are verified against rm before any byte reaches gunzip. All
// filesystem writes go through staging (*os.Root). Limits (bytes, entries,
// depth, path length, part count, and part size) are counted independently of
// rm to defend against forged manifests and decompression bombs.
//
// On any error, staging may contain partially extracted files or an escaping
// symlink detected during the post-pass; the caller must discard the staging
// directory and never commit it.
func Extract(ctx context.Context, staging *os.Root, scratchDir string, rm RootManifest, lim Limits, src PartSource) error {
	if staging == nil {
		return errors.New("bundle extract: staging root is nil")
	}
	if src == nil {
		return errors.New("bundle extract: source is nil")
	}
	if err := lim.Validate(); err != nil {
		return err
	}
	if err := rm.Validate(); err != nil {
		return err
	}
	if rm.PartSize > lim.PartSize {
		return &ErrLimitExceeded{Field: "part_size", Got: rm.PartSize, Max: lim.PartSize}
	}
	if rm.PartSize < MinManifestPartSize && rm.PartSize != lim.PartSize {
		return fmt.Errorf("%w: manifest partSize %d below minimum %d", ErrInvalidManifest, rm.PartSize, MinManifestPartSize)
	}
	if maxParts := MaxPartsFor(lim.MaxBytes, rm.PartSize); len(rm.Parts) > maxParts {
		return &ErrLimitExceeded{Field: "parts", Got: int64(len(rm.Parts)), Max: int64(maxParts)}
	}
	for i, p := range rm.Parts {
		if p.Size > rm.PartSize {
			return &ErrLimitExceeded{Field: "part_size", Got: p.Size, Max: rm.PartSize}
		}
		if i < len(rm.Parts)-1 && p.Size != rm.PartSize {
			return fmt.Errorf("%w: non-final part %d has size %d, want %d", ErrPartSizeMismatch, p.Index, p.Size, rm.PartSize)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	vpr := newVerifiedPartReader(ctx, scratchDir, rm.Parts, src)
	defer vpr.Close()

	gz, err := gzip.NewReader(vpr)
	if err != nil {
		return fmt.Errorf("open gzip stream: %w", err)
	}
	// A bundle root is strictly one gzip member; disallow concatenated
	// multistream members so trailing parts/bytes are caught by VerifyAllConsumed.
	gz.Multistream(false)
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)

	seen := make(map[string]struct{})
	// Bound total path bytes held in seen, symlinks, and dirMeta so a small
	// compressed archive with many long paths cannot exhaust reader memory.
	maxPathBytes := maxPathBytesBudget(lim)
	var (
		entries    int64
		totalBytes int64
		pathBytes  int64
		dirMeta    []deferredDirMeta
		symlinks   []extractedSymlink
	)
	copyBuf := make([]byte, 32*1024)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return fmt.Errorf("%w: %v", ErrTruncatedStream, err)
			}
			return fmt.Errorf("read tar header: %w", err)
		}

		name := hdr.Name
		if hdr.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if err := transfer.ValidateRelPath(name); err != nil {
			return fmt.Errorf("invalid archive path %q: %w", hdr.Name, err)
		}
		if len(name) > lim.MaxPathLen {
			return &ErrLimitExceeded{Field: "path_len", Got: int64(len(name)), Max: int64(lim.MaxPathLen)}
		}
		if depth := pathDepth(name); depth > lim.MaxDepth {
			return &ErrLimitExceeded{Field: "depth", Got: int64(depth), Max: int64(lim.MaxDepth)}
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("%w: %s", ErrDuplicateEntry, name)
		}
		seen[name] = struct{}{}

		if entries+1 > lim.MaxEntries {
			return &ErrLimitExceeded{Field: "entries", Got: entries + 1, Max: lim.MaxEntries}
		}
		linkTarget := ""
		if hdr.Typeflag == tar.TypeSymlink {
			linkTarget = hdr.Linkname
		}
		entryPathBytes := entryPathBytesLen(name, linkTarget)
		if pathBytes+entryPathBytes > maxPathBytes {
			return &ErrLimitExceeded{Field: "pathBytes", Got: pathBytes + entryPathBytes, Max: maxPathBytes}
		}
		entries++
		pathBytes += entryPathBytes

		if err := ensureParentDirs(staging, name); err != nil {
			return err
		}
		osName := filepath.FromSlash(name)

		switch hdr.Typeflag {
		case tar.TypeDir:
			perm := fs.FileMode(hdr.Mode) & 0o777
			workPerm := perm | 0o700
			fi, err := staging.Lstat(osName)
			if err == nil {
				if !fi.IsDir() {
					return fmt.Errorf("%w: %s already exists as non-directory", ErrDuplicateEntry, name)
				}
			} else if os.IsNotExist(err) {
				if err := staging.Mkdir(osName, workPerm); err != nil {
					return fmt.Errorf("mkdir %s: %w", name, err)
				}
			} else {
				return fmt.Errorf("lstat %s: %w", name, err)
			}
			needsChmod := perm != workPerm
			if !needsChmod {
				if err := staging.Chmod(osName, perm); err != nil { //winsec:not-protection restores portable archive permission bits
					return fmt.Errorf("chmod dir %s: %w", name, err)
				}
			}
			if needsChmod || !hdr.ModTime.IsZero() {
				dirMeta = append(dirMeta, deferredDirMeta{
					osName:     osName,
					depth:      pathDepth(name),
					perm:       perm,
					needsChmod: needsChmod,
					modTime:    hdr.ModTime,
				})
			}

		case tar.TypeReg:
			if hdr.Size < 0 {
				return fmt.Errorf("negative file size for %s: %d", name, hdr.Size)
			}
			if totalBytes+hdr.Size > lim.MaxBytes {
				return &ErrLimitExceeded{Field: "bytes", Got: totalBytes + hdr.Size, Max: lim.MaxBytes}
			}
			perm := fs.FileMode(hdr.Mode) & 0o777
			f, err := rootopen.OpenFileForWrite(staging, name, uint32(perm|0o600), false)
			if err != nil {
				return fmt.Errorf("create file %s: %w", name, err)
			}
			remaining := lim.MaxBytes - totalBytes
			n, copyErr := io.CopyBuffer(f, io.LimitReader(tr, remaining+1), copyBuf)
			closeErr := f.Close()
			if copyErr != nil {
				if errors.Is(copyErr, io.ErrUnexpectedEOF) {
					return fmt.Errorf("%w: %v", ErrTruncatedStream, copyErr)
				}
				return fmt.Errorf("extract file %s: %w", name, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close extracted file %s: %w", name, closeErr)
			}
			if n > remaining {
				return &ErrLimitExceeded{Field: "bytes", Got: totalBytes + n, Max: lim.MaxBytes}
			}
			if n != hdr.Size {
				return fmt.Errorf("%w: %s extracted %d of %d bytes", ErrTruncatedStream, name, n, hdr.Size)
			}
			if err := staging.Chmod(osName, perm); err != nil { //winsec:not-protection restores portable archive permission bits
				return fmt.Errorf("chmod file %s: %w", name, err)
			}
			if !hdr.ModTime.IsZero() {
				if err := staging.Chtimes(osName, hdr.ModTime, hdr.ModTime); err != nil {
					return fmt.Errorf("chtimes file %s: %w", name, err)
				}
			}
			totalBytes += n

		case tar.TypeSymlink:
			if err := validateSymlinkTarget(name, hdr.Linkname); err != nil {
				return fmt.Errorf("%w: %s -> %s", err, name, hdr.Linkname)
			}
			if err := staging.Symlink(hdr.Linkname, osName); err != nil {
				return fmt.Errorf("symlink %s: %w", name, err)
			}
			symlinks = append(symlinks, extractedSymlink{relPath: name, target: hdr.Linkname})

		default:
			return fmt.Errorf("%w: %s (type %q)", ErrUnsupportedEntryType, name, hdr.Typeflag)
		}
	}

	// Read the gzip stream to EOF through a 10 KiB bound (sufficient for tar
	// record padding) so gzip.Reader actually reaches and verifies the CRC32
	// and ISIZE trailer. Reject excess trailing bytes or any non-zero byte
	// after the tar end-of-archive marker.
	const maxTarTrailingZeros = 10 * 1024
	tail, err := io.ReadAll(io.LimitReader(gz, maxTarTrailingZeros+1))
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%w: %v", ErrTruncatedStream, err)
		}
		return fmt.Errorf("verify gzip trailer: %w", err)
	}
	if len(tail) > maxTarTrailingZeros {
		return fmt.Errorf("%w: excess trailing data after tar end-of-archive", ErrInvalidManifest)
	}
	for _, b := range tail {
		if b != 0 {
			return fmt.Errorf("%w: non-zero trailing data after tar end-of-archive", ErrInvalidManifest)
		}
	}
	if err := gz.Close(); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%w: %v", ErrTruncatedStream, err)
		}
		return fmt.Errorf("close gzip reader: %w", err)
	}
	if err := vpr.VerifyAllConsumed(); err != nil {
		return err
	}

	// Post-pass over all extracted symlinks to catch multi-hop symlink chains
	// (including links created before the link they traverse) that resolve
	// outside staging.
	for _, s := range symlinks {
		if err := validateSymlinkInRoot(staging, s.relPath, s.target); err != nil {
			return fmt.Errorf("%w: %s -> %s", err, s.relPath, s.target)
		}
	}

	// Apply deferred directory mtimes and read-only modes deepest-first so
	// child extraction never clobbers parent mtimes or hits EACCES.
	sort.Slice(dirMeta, func(i, j int) bool {
		return dirMeta[i].depth > dirMeta[j].depth
	})
	for _, dm := range dirMeta {
		if !dm.modTime.IsZero() {
			if err := staging.Chtimes(dm.osName, dm.modTime, dm.modTime); err != nil {
				return fmt.Errorf("chtimes dir %s: %w", dm.osName, err)
			}
		}
		if dm.needsChmod {
			if err := staging.Chmod(dm.osName, dm.perm); err != nil { //winsec:not-protection restores portable archive permission bits
				return fmt.Errorf("chmod dir %s: %w", dm.osName, err)
			}
		}
	}

	return nil
}

type deferredDirMeta struct {
	osName     string
	depth      int
	perm       fs.FileMode
	needsChmod bool
	modTime    time.Time
}

type extractedSymlink struct {
	relPath string
	target  string
}

// ensureParentDirs verifies that every ancestor component of rel inside staging
// is a real directory (never a symlink or regular file), creating missing
// intermediate directories with 0755.
func ensureParentDirs(staging *os.Root, rel string) error {
	dir := path.Dir(rel)
	if dir == "." {
		return nil
	}
	parts := strings.Split(dir, "/")
	cur := ""
	for _, p := range parts {
		if cur == "" {
			cur = p
		} else {
			cur = cur + "/" + p
		}
		osCur := filepath.FromSlash(cur)
		fi, err := staging.Lstat(osCur)
		if err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%w: %s", ErrSymlinkParent, cur)
			}
			continue
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("lstat parent %s: %w", cur, err)
		}
		if err := staging.Mkdir(osCur, 0o755); err != nil {
			return fmt.Errorf("mkdir parent %s: %w", cur, err)
		}
	}
	return nil
}

// verifiedPartReader streams parts in order, downloading each part to a temp
// file in scratchDir and verifying its exact size and SHA-256 digest before
// exposing any byte to callers (such as gzip.NewReader).
type verifiedPartReader struct {
	ctx        context.Context
	scratchDir string
	parts      []Part
	src        PartSource

	nextIdx int
	curFile *os.File
	curPath string
	copyBuf []byte
}

func newVerifiedPartReader(ctx context.Context, scratchDir string, parts []Part, src PartSource) *verifiedPartReader {
	return &verifiedPartReader{
		ctx:        ctx,
		scratchDir: scratchDir,
		parts:      parts,
		src:        src,
		copyBuf:    make([]byte, 32*1024),
	}
}

func (r *verifiedPartReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	for {
		if r.curFile == nil {
			if r.nextIdx >= len(r.parts) {
				return 0, io.EOF
			}
			if err := r.loadNextPart(); err != nil {
				return 0, err
			}
		}
		n, err := r.curFile.Read(p)
		if errors.Is(err, io.EOF) {
			r.closeCur()
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (r *verifiedPartReader) loadNextPart() error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	part := r.parts[r.nextIdx]
	rc, err := r.src.Get(r.ctx, part)
	if err != nil {
		return fmt.Errorf("get part %d: %w", part.Index, err)
	}
	defer func() { _ = rc.Close() }()

	f, err := os.CreateTemp(r.scratchDir, "scion-bundle-read-*")
	if err != nil {
		return fmt.Errorf("create temp part file: %w", err)
	}
	tmpPath := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}

	hasher := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(f, hasher), io.LimitReader(rc, part.Size+1), r.copyBuf)
	if err != nil {
		cleanup()
		return fmt.Errorf("download part %d: %w", part.Index, err)
	}
	if n != part.Size {
		cleanup()
		return fmt.Errorf("%w: part %d got %d bytes, want %d", ErrPartSizeMismatch, part.Index, n, part.Size)
	}
	gotHash := transfer.HashPrefix + hex.EncodeToString(hasher.Sum(nil))
	if gotHash != part.Hash {
		cleanup()
		return fmt.Errorf("%w: part %d got %s, want %s", ErrHashMismatch, part.Index, gotHash, part.Hash)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return fmt.Errorf("seek part %d: %w", part.Index, err)
	}

	r.curFile = f
	r.curPath = tmpPath
	r.nextIdx++
	return nil
}

func (r *verifiedPartReader) closeCur() {
	if r.curFile != nil {
		_ = r.curFile.Close()
		_ = os.Remove(r.curPath)
		r.curFile = nil
		r.curPath = ""
	}
}

// VerifyAllConsumed ensures no parts remain unread after the tar/gzip stream
// finishes, and closes any remaining temp file.
func (r *verifiedPartReader) VerifyAllConsumed() error {
	if r.curFile != nil {
		// Check if current file is at EOF.
		var buf [1]byte
		n, err := r.curFile.Read(buf[:])
		r.closeCur()
		if n > 0 {
			return fmt.Errorf("%w: trailing data in part %d", ErrInvalidManifest, r.nextIdx-1)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	if r.nextIdx < len(r.parts) {
		return fmt.Errorf("%w: %d unconsumed trailing parts", ErrInvalidManifest, len(r.parts)-r.nextIdx)
	}
	return nil
}

func (r *verifiedPartReader) Close() {
	r.closeCur()
}
