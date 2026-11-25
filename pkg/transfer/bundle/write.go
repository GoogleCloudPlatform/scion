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
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

// Write streams root into a gzip-compressed tar archive split into parts of at
// most lim.PartSize compressed bytes, passing each completed part to sink.Put.
//
// Temporary part files are created in scratchDir (never inside root; empty
// defaults to os.TempDir) and deleted immediately after each Put or on abort.
// Write aborts immediately with *ErrLimitExceeded when any bound in lim is
// crossed, with no further sink.Put calls.
func Write(ctx context.Context, root *os.Root, scratchDir string, ex Excluder, lim Limits, sink PartSink) (RootManifest, error) {
	if root == nil {
		return RootManifest{}, errors.New("bundle write: root is nil")
	}
	if sink == nil {
		return RootManifest{}, errors.New("bundle write: sink is nil")
	}
	if err := lim.Validate(); err != nil {
		return RootManifest{}, err
	}
	if err := ctx.Err(); err != nil {
		return RootManifest{}, err
	}

	ex, excludedInodes, err := resolveExcluderInRoot(root, ex)
	if err != nil {
		return RootManifest{}, err
	}

	splitter := newPartSplitter(ctx, scratchDir, lim.PartSize, lim.MaxParts(), sink)
	defer splitter.Abort()

	gw := gzip.NewWriter(splitter)
	tw := tar.NewWriter(gw)

	var (
		entries    int64
		totalBytes int64
		pathBytes  int64
		skipped    SkipCounts
	)
	copyBuf := make([]byte, 32*1024)

	walkErr := transfer.WalkRoot(root, func(rel string, d fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		kind, linkTarget, err := classifyEntry(root, rel, d, ex, excludedInodes, entries, pathBytes, lim, &skipped)
		if err != nil {
			return err
		}
		switch kind {
		case entrySkip:
			return nil

		case entrySymlink:
			hdr := &tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     rel,
				Linkname: linkTarget,
				Mode:     0o777,
				Format:   tar.FormatPAX,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("write symlink header %s: %w", rel, err)
			}
			entries++
			pathBytes += entryPathBytesLen(rel, linkTarget)
			return nil

		case entryDir:
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("stat dir %s: %w", rel, err)
			}
			perm := info.Mode().Perm() & 0o777
			hdr := &tar.Header{
				Typeflag: tar.TypeDir,
				Name:     rel + "/",
				Mode:     int64(perm),
				ModTime:  info.ModTime().UTC(),
				Format:   tar.FormatPAX,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("write dir header %s: %w", rel, err)
			}
			entries++
			pathBytes += entryPathBytesLen(rel, "")
			return nil

		case entryReg:
			f, info, err := transfer.OpenRegularFile(root, filepath.FromSlash(rel))
			if err != nil {
				return fmt.Errorf("open regular file %s: %w", rel, err)
			}
			size := info.Size()
			if totalBytes+size > lim.MaxBytes {
				_ = f.Close()
				return &ErrLimitExceeded{Field: "bytes", Got: totalBytes + size, Max: lim.MaxBytes}
			}

			perm := info.Mode().Perm() & 0o777
			hdr := &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     rel,
				Size:     size,
				Mode:     int64(perm),
				ModTime:  info.ModTime().UTC(),
				Format:   tar.FormatPAX,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				_ = f.Close()
				return fmt.Errorf("write file header %s: %w", rel, err)
			}
			n, err := io.CopyBuffer(tw, io.LimitReader(f, size), copyBuf)
			closeErr := f.Close()
			if err != nil {
				return fmt.Errorf("copy file %s: %w", rel, err)
			}
			if n != size {
				return fmt.Errorf("short read on %s: copied %d of %d bytes", rel, n, size)
			}
			if closeErr != nil {
				return fmt.Errorf("close file %s: %w", rel, closeErr)
			}
			entries++
			totalBytes += size
			pathBytes += entryPathBytesLen(rel, "")
			return nil
		}
		return nil
	})
	if walkErr != nil {
		return RootManifest{}, walkErr
	}

	if err := tw.Close(); err != nil {
		return RootManifest{}, fmt.Errorf("close tar writer: %w", err)
	}
	if err := gw.Close(); err != nil {
		return RootManifest{}, fmt.Errorf("close gzip writer: %w", err)
	}
	if err := splitter.Close(); err != nil {
		return RootManifest{}, err
	}

	return RootManifest{
		Parts:    splitter.parts,
		PartSize: lim.PartSize,
		Entries:  entries,
		Bytes:    totalBytes,
		Skipped:  skipped,
	}, nil
}

// EstimateOptions configures optional checks performed during EstimateWithOptions.
type EstimateOptions struct {
	// CheckReadable, when true, verifies that every non-excluded regular file
	// encountered within limits can be opened for reading via rootopen, failing
	// closed with fs.ErrPermission on unreadable files (e.g. pod-owned 0600
	// files on root-squash NFS exports) without exceeding Limits or walking
	// excluded subtrees.
	CheckReadable bool
}

// Estimate performs a stat-only walk of root using the same excluder, symlink
// rules, and limits as Write, stopping early with *ErrLimitExceeded if any
// bound is crossed.
func Estimate(ctx context.Context, root *os.Root, ex Excluder, lim Limits) (EstimateResult, error) {
	return EstimateWithOptions(ctx, root, ex, lim, EstimateOptions{})
}

// EstimateWithOptions performs a bounded walk of root using the same excluder,
// symlink rules, and limits as Write, with optional extra checks from opts.
func EstimateWithOptions(ctx context.Context, root *os.Root, ex Excluder, lim Limits, opts EstimateOptions) (EstimateResult, error) {
	if root == nil {
		return EstimateResult{}, errors.New("bundle estimate: root is nil")
	}
	if err := lim.Validate(); err != nil {
		return EstimateResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return EstimateResult{}, err
	}

	ex, excludedInodes, err := resolveExcluderInRoot(root, ex)
	if err != nil {
		return EstimateResult{}, err
	}

	var (
		res       EstimateResult
		pathBytes int64
	)
	err = transfer.WalkRoot(root, func(rel string, d fs.DirEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		kind, linkTarget, err := classifyEntry(root, rel, d, ex, excludedInodes, res.Entries, pathBytes, lim, &res.Skipped)
		if err != nil {
			return err
		}
		switch kind {
		case entrySkip:
			return nil
		case entrySymlink:
			res.Entries++
			pathBytes += entryPathBytesLen(rel, linkTarget)
			return nil
		case entryDir:
			res.Entries++
			pathBytes += entryPathBytesLen(rel, "")
			return nil
		case entryReg:
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("stat %s: %w", rel, err)
			}
			if res.Bytes+info.Size() > lim.MaxBytes {
				return &ErrLimitExceeded{Field: "bytes", Got: res.Bytes + info.Size(), Max: lim.MaxBytes}
			}
			if opts.CheckReadable {
				f, _, openErr := rootopen.OpenRegularForRead(root, filepath.FromSlash(rel), nil)
				if openErr != nil {
					return fmt.Errorf("read %s: %w", rel, openErr)
				}
				_ = f.Close()
			}
			res.Entries++
			res.Bytes += info.Size()
			pathBytes += entryPathBytesLen(rel, "")
			return nil
		}
		return nil
	})
	if err != nil {
		return EstimateResult{}, err
	}
	return res, nil
}

type entryKind int

const (
	entrySkip entryKind = iota
	entryDir
	entryReg
	entrySymlink
)

// classifyEntry applies exclusion, hardlink-to-excluded-file skipping,
// special-file skipping, symlink chain containment validation, and
// path/depth/entry-count/pathBytes limits to one walked entry. Both Write and
// Estimate use it so their rules stay identical.
func classifyEntry(root *os.Root, rel string, d fs.DirEntry, ex Excluder, excludedInodes excludedInodeSet, currentEntries, currentPathBytes int64, lim Limits, skipped *SkipCounts) (entryKind, string, error) {
	if ex != nil && ex.Exclude(rel, d.IsDir()) {
		skipped.Excluded++
		if d.IsDir() {
			return entrySkip, "", fs.SkipDir
		}
		return entrySkip, "", nil
	}

	modeType := d.Type()
	switch {
	case modeType&fs.ModeSocket != 0:
		skipped.Sockets++
		return entrySkip, "", nil
	case modeType&fs.ModeNamedPipe != 0:
		skipped.FIFOs++
		return entrySkip, "", nil
	case modeType&(fs.ModeDevice|fs.ModeCharDevice) != 0:
		skipped.Devices++
		return entrySkip, "", nil
	}

	if modeType&fs.ModeSymlink != 0 {
		target, err := root.Readlink(filepath.FromSlash(rel))
		if err != nil {
			return entrySkip, "", fmt.Errorf("readlink %s: %w", rel, err)
		}
		if err := validateSymlinkInRoot(root, rel, target); err != nil {
			skipped.EscapingSymlinks++
			return entrySkip, "", nil
		}
		if err := checkPathAndEntryLimits(rel, target, currentEntries, currentPathBytes, lim); err != nil {
			return entrySkip, "", err
		}
		return entrySymlink, target, nil
	}

	if modeType.IsRegular() && excludedInodes.contains(d) {
		skipped.Excluded++
		return entrySkip, "", nil
	}

	if err := checkPathAndEntryLimits(rel, "", currentEntries, currentPathBytes, lim); err != nil {
		return entrySkip, "", err
	}

	if d.IsDir() {
		return entryDir, "", nil
	}
	if !modeType.IsRegular() {
		// Any other non-regular type (e.g. ModeIrregular) is skipped.
		skipped.Devices++
		return entrySkip, "", nil
	}
	return entryReg, "", nil
}

func checkPathAndEntryLimits(rel, linkTarget string, currentEntries, currentPathBytes int64, lim Limits) error {
	if err := transfer.ValidateRelPath(rel); err != nil {
		return &ErrUnportablePath{Path: rel, Err: err}
	}
	if len(rel) > lim.MaxPathLen {
		return &ErrLimitExceeded{Field: "path_len", Got: int64(len(rel)), Max: int64(lim.MaxPathLen)}
	}
	if depth := pathDepth(rel); depth > lim.MaxDepth {
		return &ErrLimitExceeded{Field: "depth", Got: int64(depth), Max: int64(lim.MaxDepth)}
	}
	if currentEntries+1 > lim.MaxEntries {
		return &ErrLimitExceeded{Field: "entries", Got: currentEntries + 1, Max: lim.MaxEntries}
	}
	maxPB := maxPathBytesBudget(lim)
	nextPB := currentPathBytes + entryPathBytesLen(rel, linkTarget)
	if nextPB > maxPB {
		return &ErrLimitExceeded{Field: "pathBytes", Got: nextPB, Max: maxPB}
	}
	return nil
}

// partSplitter splits an arbitrary byte stream into temporary files of at most
// partSize bytes in scratchDir, computing SHA-256 on the fly and handing each
// finished part to sink.Put before removing the temp file.
type partSplitter struct {
	ctx        context.Context
	scratchDir string
	partSize   int64
	maxParts   int
	sink       PartSink

	index   int
	parts   []Part
	curFile *os.File
	curPath string
	hasher  hash.Hash
	written int64
}

func newPartSplitter(ctx context.Context, scratchDir string, partSize int64, maxParts int, sink PartSink) *partSplitter {
	return &partSplitter{
		ctx:        ctx,
		scratchDir: scratchDir,
		partSize:   partSize,
		maxParts:   maxParts,
		sink:       sink,
		hasher:     sha256.New(),
	}
}

func (s *partSplitter) openPart() error {
	if s.index >= s.maxParts {
		return &ErrLimitExceeded{Field: "parts", Got: int64(s.index + 1), Max: int64(s.maxParts)}
	}
	f, err := os.CreateTemp(s.scratchDir, "scion-bundle-write-*")
	if err != nil {
		return fmt.Errorf("create temp part file: %w", err)
	}
	s.curFile = f
	s.curPath = f.Name()
	s.hasher.Reset()
	s.written = 0
	return nil
}

func (s *partSplitter) Write(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	total := 0
	for len(p) > 0 {
		if s.curFile == nil {
			if err := s.openPart(); err != nil {
				return total, err
			}
		}
		rem := s.partSize - s.written
		chunk := p
		if int64(len(chunk)) > rem {
			chunk = p[:rem]
		}
		n, err := s.curFile.Write(chunk)
		if n > 0 {
			_, _ = s.hasher.Write(chunk[:n])
			s.written += int64(n)
			total += n
			p = p[n:]
		}
		if err != nil {
			return total, fmt.Errorf("write temp part file: %w", err)
		}
		if s.written == s.partSize {
			if err := s.flushPart(); err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

func (s *partSplitter) flushPart() error {
	if s.curFile == nil {
		return nil
	}
	f := s.curFile
	path := s.curPath
	size := s.written
	digest := transfer.HashPrefix + hex.EncodeToString(s.hasher.Sum(nil))
	idx := s.index

	// Clear state and always clean up the temp file when flushPart returns.
	s.curFile = nil
	s.curPath = ""
	s.written = 0
	s.hasher.Reset()
	defer func() {
		_ = f.Close()
		_ = os.Remove(path)
	}()

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek temp part %d: %w", idx, err)
	}
	if err := s.sink.Put(s.ctx, idx, f, size, digest); err != nil {
		return fmt.Errorf("put part %d: %w", idx, err)
	}
	s.parts = append(s.parts, Part{
		Index: idx,
		Size:  size,
		Hash:  digest,
	})
	s.index++
	return nil
}

func (s *partSplitter) Close() error {
	if s.curFile != nil && s.written > 0 {
		return s.flushPart()
	}
	s.Abort()
	return nil
}

func (s *partSplitter) Abort() {
	if s.curFile != nil {
		_ = s.curFile.Close()
		_ = os.Remove(s.curPath)
		s.curFile = nil
		s.curPath = ""
		s.written = 0
	}
}
