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

package githubfetch

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha1" //nolint:gosec // git blob object IDs are SHA-1
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

// SkipCounts records non-fatal entries skipped during extraction.
type SkipCounts struct {
	Symlinks      int
	Hardlinks     int
	Submodules    int
	DotGit        int
	ExportIgnored int
	// Other counts non-regular, non-directory entries (such as devices or
	// FIFOs) outside the selected subtrees in selector mode.
	Other int
}

var (
	errUnpackedLimit = errors.New("unpacked archive stream exceeds limit")

	// ErrPAXMissing is wrapped in a KindUpstream Error when an archive has no
	// leading PAX global commit-ID header and RequireLeadingPAX is set.
	ErrPAXMissing = errors.New("archive has no commit id header")

	// ErrPAXMismatch is wrapped in a KindUpstream Error when an archive's PAX
	// global commit-ID header does not match the expected commit SHA.
	ErrPAXMismatch = errors.New("archive PAX commit SHA does not match resolved commit")
)

// ExtractBudget tracks cumulative file and byte counts across one or more
// ExtractTarGz calls against Limits.MaxFiles and Limits.MaxTotal.
type ExtractBudget struct {
	Files int
	Bytes int64
}

// TarExtractOptions configures ExtractTarGz for callers that supply their own
// gzipped GitHub tarball stream (such as welcome staging).
type TarExtractOptions struct {
	// Repo is the "owner/repo" slug included on typed Errors.
	Repo string
	// SubPath extracts entries under <top>/<SubPath> when Select is nil.
	SubPath string
	// ExpectedSHA, when non-empty, is verified against the archive's PAX
	// global header comment.
	ExpectedSHA string
	// Tree, when non-nil and not truncated, is verified against the written
	// files and their git blob SHAs (SubPath mode only).
	Tree *Tree
	// Limits bounds the extraction.
	Limits Limits
	// RequireLeadingPAX requires the very first tar entry to be a PAX global
	// header with a non-empty comment matching ExpectedSHA (if set), before
	// any file is written, and rejects any subsequent PAX global header.
	RequireLeadingPAX bool
	// Select, when non-nil, switches extraction to selector mode: every tar
	// entry path is strictly validated (relative, no "."/"..", single top-level
	// directory), and Select maps an archive-relative path (top-level dir
	// removed) to zero or more root-relative destination paths.
	Select func(rel string) []string
	// Roots lists archive-relative selection root directories (no trailing
	// slash) in selector mode. Any non-directory, non-regular entry at or
	// above a root is treated as inside the selection.
	Roots []string
	// SkipEmptyDirs skips standalone TypeDir entries (parent directories are
	// still created as needed when writing regular files).
	SkipEmptyDirs bool
	// DirMode is the permission mode used when creating directories; zero
	// defaults to 0o755.
	DirMode os.FileMode
	// Budget, when non-nil, enforces and updates cumulative file and byte
	// totals across multiple ExtractTarGz calls.
	Budget *ExtractBudget
}

// ExtractResult reports the files, bytes, and skipped entries for one
// ExtractTarGz call.
type ExtractResult struct {
	Files   int
	Bytes   int64
	Skipped SkipCounts
}

// extractTarGz streams a GitHub gzip tarball in a single pass, keeping only
// entries under subPath, writing them through dst (*os.Root) with O_EXCL and
// normalized 0644/0755 modes, hashing each written file's git blob SHA on the
// fly, and verifying the PAX global commit SHA and (when tree is non-nil and
// not truncated) the exact file set and blob SHAs against tree.
func extractTarGz(r io.Reader, repo, subPath, expectedSHA string, tree *Tree, dst *os.Root, l Limits) (int, int64, SkipCounts, error) {
	res, err := ExtractTarGz(r, dst, TarExtractOptions{
		Repo:        repo,
		SubPath:     subPath,
		ExpectedSHA: expectedSHA,
		Tree:        tree,
		Limits:      l,
	})
	return res.Files, res.Bytes, res.Skipped, err
}

// ExtractTarGz streams a GitHub gzip tarball into dst (*os.Root) in a single
// pass, enforcing opts.Limits and archive safety rules.
func ExtractTarGz(r io.Reader, dst *os.Root, opts TarExtractOptions) (ExtractResult, error) {
	l := opts.Limits.withDefaults()
	repo := opts.Repo
	dirMode := opts.DirMode
	if dirMode == 0 {
		dirMode = 0o755
	}

	gzr, err := gzip.NewReader(r)
	if err != nil {
		var ghe *Error
		if errors.As(err, &ghe) {
			return ExtractResult{}, ghe
		}
		return ExtractResult{}, &Error{Kind: KindUpstream, Repo: repo, Hint: "invalid gzip archive", err: err}
	}
	defer func() { _ = gzr.Close() }()

	if l.MaxUnpacked <= 0 {
		return ExtractResult{}, &Error{Kind: KindTooLarge, Repo: repo, Hint: "archive extraction requires a positive MaxUnpacked limit"}
	}
	tarStream := &streamCapReader{r: gzr, remaining: l.MaxUnpacked, tooLargeErr: errUnpackedLimit}
	tr := tar.NewReader(tarStream)

	classifyReadErr := func(err error) error {
		if errors.Is(err, errUnpackedLimit) {
			return &Error{Kind: KindTooLarge, Repo: repo, Hint: "decompressed archive exceeds MaxUnpacked"}
		}
		var ghe *Error
		if errors.As(err, &ghe) {
			return ghe
		}
		return ClassifyRemoteFetchError(err, repo, Credential{}, nowUTC())
	}

	var (
		paxSHA      string
		top         string
		found       bool
		dirs        int
		callFiles   int
		callWritten int64
		skipped     SkipCounts
		actualBlobs = make(map[string]string)
	)
	budget := opts.Budget
	if budget == nil {
		budget = &ExtractBudget{}
	}

	if opts.RequireLeadingPAX {
		first, err := tr.Next()
		if err == io.EOF {
			return ExtractResult{}, &Error{Kind: KindUpstream, Repo: repo, Hint: ErrPAXMissing.Error(), err: ErrPAXMissing}
		}
		if err != nil {
			return ExtractResult{}, classifyReadErr(err)
		}
		if first.Typeflag != tar.TypeXGlobalHeader {
			return ExtractResult{}, &Error{Kind: KindUpstream, Repo: repo, Hint: ErrPAXMissing.Error(), err: ErrPAXMissing}
		}
		comment := strings.TrimSpace(first.PAXRecords["comment"])
		if comment == "" {
			return ExtractResult{}, &Error{Kind: KindUpstream, Repo: repo, Hint: ErrPAXMissing.Error(), err: ErrPAXMissing}
		}
		if opts.ExpectedSHA != "" && !strings.EqualFold(comment, opts.ExpectedSHA) {
			return ExtractResult{}, &Error{Kind: KindUpstream, Repo: repo, Hint: ErrPAXMismatch.Error(), err: ErrPAXMismatch}
		}
		paxSHA = strings.ToLower(comment)
	}

	subPath := strings.Trim(opts.SubPath, "/")
	selectorMode := opts.Select != nil

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ExtractResult{Skipped: skipped}, classifyReadErr(err)
		}

		if hdr.Typeflag == tar.TypeXGlobalHeader {
			if opts.RequireLeadingPAX {
				return ExtractResult{Skipped: skipped}, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "archive contains a second PAX global header"}
			}
			if paxSHA == "" {
				cand := strings.TrimSpace(hdr.PAXRecords["comment"])
				if IsFullCommitSHA(cand) {
					paxSHA = cand
				}
			}
			continue
		}

		var dests []string
		if selectorMode {
			rel, err := parseStrictTarPath(hdr.Name, &top, repo)
			if err != nil {
				return ExtractResult{Skipped: skipped}, err
			}
			if rel == "" {
				if hdr.Typeflag != tar.TypeDir {
					return ExtractResult{Skipped: skipped}, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "archive top-level entry is not a directory"}
				}
				continue
			}

			dests = opts.Select(rel)
			switch hdr.Typeflag {
			case tar.TypeDir:
				if opts.SkipEmptyDirs || len(dests) == 0 {
					continue
				}
				for _, dest := range dests {
					if err := validateDestPath(dest, l.MaxNameBytes, repo); err != nil {
						return ExtractResult{Skipped: skipped}, err
					}
					dirs++
					if l.MaxFiles > 0 && dirs > 4*l.MaxFiles {
						return ExtractResult{Skipped: skipped}, &Error{Kind: KindTooLarge, Repo: repo, Hint: "archive exceeds directory limit"}
					}
					d, err := rootopen.MkdirAll(dst, dest, uint32(dirMode))
					if err != nil {
						return ExtractResult{Skipped: skipped}, err
					}
					_ = d.Close()
				}
				continue
			case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA is read as a regular file
				if len(dests) == 0 {
					continue
				}
			default:
				selected := len(dests) > 0 || len(opts.Select(rel+"/x")) > 0 || onRootPath(opts.Roots, rel)
				if selected {
					switch hdr.Typeflag {
					case tar.TypeSymlink:
						if l.OnSymlink == PolicyReject {
							return ExtractResult{Skipped: skipped}, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "archive contains a symlink"}
						}
						skipped.Symlinks++
						continue
					case tar.TypeLink:
						if l.OnSymlink == PolicyReject {
							return ExtractResult{Skipped: skipped}, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "archive contains a hardlink"}
						}
						skipped.Hardlinks++
						continue
					default:
						return ExtractResult{Skipped: skipped}, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "archive contains a device, FIFO, or unsupported entry"}
					}
				}
				switch hdr.Typeflag {
				case tar.TypeSymlink:
					skipped.Symlinks++
				case tar.TypeLink:
					skipped.Hardlinks++
				default:
					skipped.Other++
				}
				continue
			}
		} else {
			// Decide whether the entry falls inside subPath on the raw tar name
			// first, skipping out-of-scope entries so an unusual filename or
			// device elsewhere in the repository does not abort a sub-path fetch.
			rel, inSub, err := stripTarPath(hdr.Name, subPath, repo)
			if err != nil {
				return ExtractResult{Skipped: skipped}, err
			}
			if !inSub {
				continue
			}

			// Devices and FIFOs inside the fetched sub-path are always refused.
			switch hdr.Typeflag {
			case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
				return ExtractResult{Skipped: skipped}, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "archive contains a device or FIFO entry"}
			}

			if rel != "" && hasDotGitSegment(rel) {
				skipped.DotGit++
				continue
			}

			switch hdr.Typeflag {
			case tar.TypeSymlink:
				if l.OnSymlink == PolicyReject {
					return ExtractResult{Skipped: skipped}, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "archive contains a symlink"}
				}
				skipped.Symlinks++
				continue
			case tar.TypeLink:
				if l.OnSymlink == PolicyReject {
					return ExtractResult{Skipped: skipped}, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "archive contains a hardlink"}
				}
				skipped.Hardlinks++
				continue
			case tar.TypeDir, tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA is read as a regular file
				// Handled below.
			default:
				continue
			}

			found = true
			if rel == "" {
				continue
			}
			if len(rel) > l.MaxNameBytes {
				return ExtractResult{Skipped: skipped}, &Error{Kind: KindTooLarge, Repo: repo, Hint: "archive entry path exceeds MaxNameBytes"}
			}

			if hdr.Typeflag == tar.TypeDir {
				if opts.SkipEmptyDirs {
					continue
				}
				dirs++
				if l.MaxFiles > 0 && dirs > 4*l.MaxFiles {
					return ExtractResult{Skipped: skipped}, &Error{Kind: KindTooLarge, Repo: repo, Hint: "archive exceeds directory limit"}
				}
				d, err := rootopen.MkdirAll(dst, rel, uint32(dirMode))
				if err != nil {
					return ExtractResult{Skipped: skipped}, err
				}
				_ = d.Close()
				continue
			}
			dests = []string{rel}
		}

		// Validate destination paths in selector mode.
		if selectorMode {
			for _, dest := range dests {
				if err := validateDestPath(dest, l.MaxNameBytes, repo); err != nil {
					return ExtractResult{Skipped: skipped}, err
				}
			}
		}

		// Regular file: count regular files only against MaxFiles to match
		// git tree validation; directory entries have their own cap above
		// because gitlink/empty directories can appear in tarballs without
		// being counted as blobs in ListTree.
		if hdr.Size < 0 || (l.MaxFileBytes > 0 && hdr.Size > l.MaxFileBytes) {
			return ExtractResult{Skipped: skipped}, &Error{Kind: KindTooLarge, Repo: repo, Hint: "archive file exceeds MaxFileBytes"}
		}
		if l.MaxFiles > 0 && budget.Files+len(dests) > l.MaxFiles {
			return ExtractResult{Skipped: skipped}, &Error{Kind: KindTooLarge, Repo: repo, Hint: "archive exceeds MaxFiles"}
		}
		if l.MaxTotal > 0 && hdr.Size > (l.MaxTotal-budget.Bytes)/int64(len(dests)) {
			return ExtractResult{Skipped: skipped}, &Error{Kind: KindTooLarge, Repo: repo, Hint: "extracted files exceed MaxTotal"}
		}

		mode := normalizeArchiveMode(hdr.Mode)
		openFiles := make([]*os.File, 0, len(dests))
		closeAll := func() error {
			var firstErr error
			for _, f := range openFiles {
				if err := f.Close(); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			return firstErr
		}
		for _, dest := range dests {
			if dir := path.Dir(dest); dir != "." && dir != "" {
				d, err := rootopen.MkdirAll(dst, dir, uint32(dirMode))
				if err != nil {
					_ = closeAll()
					return ExtractResult{Skipped: skipped}, err
				}
				_ = d.Close()
			}
			f, err := rootopen.OpenFileForWrite(dst, dest, uint32(mode), false)
			if err != nil {
				_ = closeAll()
				return ExtractResult{Skipped: skipped}, fmt.Errorf("create extracted file %q: %w", dest, err)
			}
			// Apply the normalized mode independent of process umask.
			if err := f.Chmod(mode); err != nil { //winsec:not-protection normalize extracted file mode to 0644/0755
				_ = f.Close()
				_ = closeAll()
				return ExtractResult{Skipped: skipped}, err
			}
			openFiles = append(openFiles, f)
		}

		writers := make([]io.Writer, 0, len(openFiles)+1)
		for _, f := range openFiles {
			writers = append(writers, f)
		}
		var blobHash interface {
			io.Writer
			Sum([]byte) []byte
		}
		if !selectorMode && opts.Tree != nil && !opts.Tree.Truncated {
			h := sha1.New() //nolint:gosec // git blob object IDs are SHA-1
			_, _ = fmt.Fprintf(h, "blob %d\x00", hdr.Size)
			writers = append(writers, h)
			blobHash = h
		}

		var src io.Reader = tr
		var readLimit int64 = -1
		if l.MaxFileBytes > 0 {
			readLimit = l.MaxFileBytes
		}
		if l.MaxTotal > 0 {
			perDestRemaining := (l.MaxTotal - budget.Bytes) / int64(len(dests))
			if readLimit < 0 || perDestRemaining < readLimit {
				readLimit = perDestRemaining
			}
		}
		if readLimit >= 0 {
			src = io.LimitReader(tr, readLimit+1)
		}

		n, copyErr := io.Copy(io.MultiWriter(writers...), src)
		closeErr := closeAll()
		budget.Files += len(dests)
		budget.Bytes += n * int64(len(dests))
		callFiles += len(dests)
		callWritten += n * int64(len(dests))
		if copyErr != nil {
			return ExtractResult{Skipped: skipped}, classifyReadErr(copyErr)
		}
		if l.MaxFileBytes > 0 && n > l.MaxFileBytes {
			return ExtractResult{Skipped: skipped}, &Error{Kind: KindTooLarge, Repo: repo, Hint: "archive file exceeds MaxFileBytes"}
		}
		if l.MaxTotal > 0 && budget.Bytes > l.MaxTotal {
			return ExtractResult{Skipped: skipped}, &Error{Kind: KindTooLarge, Repo: repo, Hint: "extracted files exceed MaxTotal"}
		}
		if closeErr != nil {
			return ExtractResult{Skipped: skipped}, closeErr
		}
		if blobHash != nil {
			actualBlobs[dests[0]] = hex.EncodeToString(blobHash.Sum(nil))
		}
	}

	if !selectorMode && !found {
		return ExtractResult{Skipped: skipped}, &Error{Kind: KindNotFound, Repo: repo, Hint: "source folder not found in archive"}
	}

	if opts.ExpectedSHA != "" && !strings.EqualFold(paxSHA, opts.ExpectedSHA) {
		return ExtractResult{Skipped: skipped}, &Error{Kind: KindUpstream, Repo: repo, Hint: ErrPAXMismatch.Error(), err: ErrPAXMismatch}
	}

	// Verify archive ⊆ tree: every extracted regular file must appear in the
	// git tree with a matching git blob SHA. Tree files absent from the archive
	// (e.g. omitted by .gitattributes export-ignore) are counted in
	// skipped.ExportIgnored rather than failing.
	//
	// Known limitation: files rewritten by git archive (such as .gitattributes
	// export-subst or LFS-content-in-archive) have different bytes than the git
	// blob SHA in the tree and fail integrity verification.
	if !selectorMode && opts.Tree != nil && !opts.Tree.Truncated {
		expected := make(map[string]File, len(opts.Tree.Files))
		for _, tf := range opts.Tree.Files {
			expected[tf.Path] = tf
		}
		for rel, gotSHA := range actualBlobs {
			tf, ok := expected[rel]
			if !ok {
				return ExtractResult{Skipped: skipped}, &Error{Kind: KindUpstream, Repo: repo, Hint: "archive contains file not listed in git tree"}
			}
			if tf.BlobSHA != "" && gotSHA != tf.BlobSHA {
				return ExtractResult{Skipped: skipped}, &Error{Kind: KindUpstream, Repo: repo, Hint: "archive file blob SHA mismatch"}
			}
		}
		for _, tf := range opts.Tree.Files {
			if _, ok := actualBlobs[tf.Path]; !ok {
				skipped.ExportIgnored++
			}
		}
	}

	return ExtractResult{Files: callFiles, Bytes: callWritten, Skipped: skipped}, nil
}

// parseStrictTarPath validates an archive entry name strictly (relative, no
// NUL or backslash, no "." or ".." or empty segments) and enforces that every
// entry shares a single top-level directory. It returns the path relative to
// that top-level directory ("" for the top-level directory itself).
func parseStrictTarPath(rawName string, top *string, repo string) (string, error) {
	if rawName == "" || strings.HasPrefix(rawName, "/") || strings.ContainsAny(rawName, "\x00\\") {
		return "", &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "unsafe archive entry path"}
	}
	trimmed := strings.TrimSuffix(rawName, "/")
	if trimmed == "" {
		return "", &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "unsafe archive entry path"}
	}
	for _, c := range strings.Split(trimmed, "/") {
		if c == "" || c == ".." || c == "." {
			return "", &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "archive contains unsafe path segment"}
		}
	}
	head, rel, _ := strings.Cut(trimmed, "/")
	if *top == "" {
		*top = head
	} else if head != *top {
		return "", &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "archive has more than one top-level directory"}
	}
	return rel, nil
}

// onRootPath reports whether rel is a selection root or an ancestor directory
// of one.
func onRootPath(roots []string, rel string) bool {
	for _, r := range roots {
		if rel == r || strings.HasPrefix(r, rel+"/") {
			return true
		}
	}
	return false
}

// validateDestPath checks that a destination path returned by Select is a
// clean, relative slash path within maxNameBytes.
func validateDestPath(dest string, maxNameBytes int, repo string) error {
	clean := path.Clean(dest)
	if dest == "" || clean != dest || path.IsAbs(clean) || strings.HasPrefix(clean, "../") || clean == ".." || strings.ContainsAny(dest, "\x00\\") {
		return &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "unsafe destination path"}
	}
	if maxNameBytes > 0 && len(dest) > maxNameBytes {
		return &Error{Kind: KindTooLarge, Repo: repo, Hint: "archive entry path exceeds MaxNameBytes"}
	}
	return nil
}

// stripTarPath decides whether rawName falls inside subPath on the raw entry
// name first, skipping out-of-scope entries, and then validates in-scope
// entries (rejecting absolute paths, backslashes, NUL bytes, "." and ".."
// segments, and empty interior segments). It strips the archive's single
// top-level directory and returns the path relative to subPath.
func stripTarPath(rawName, subPath, repo string) (string, bool, error) {
	trimmed := strings.TrimSuffix(rawName, "/")
	if subPath != "" {
		if rawName == "" || strings.HasPrefix(rawName, "/") || trimmed == "" {
			return "", false, nil
		}
		segs := strings.Split(trimmed, "/")
		subSegs := strings.Split(subPath, "/")
		if len(segs) < 1+len(subSegs) || segs[0] == "" || segs[0] == "." || segs[0] == ".." {
			return "", false, nil
		}
		if !segmentsMatchPrefix(segs[1:], subSegs) {
			return "", false, nil
		}
	}

	if rawName == "" || strings.ContainsAny(rawName, "\\\x00") {
		return "", false, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "unsafe archive entry path"}
	}
	if strings.HasPrefix(rawName, "/") {
		return "", false, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "archive contains absolute path"}
	}
	if trimmed == "" {
		return "", false, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "unsafe archive entry path"}
	}
	segs := strings.Split(trimmed, "/")
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return "", false, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "archive contains unsafe path segment"}
		}
	}
	if len(segs) == 1 {
		// Top-level repository root directory itself (e.g. "repo-sha/").
		if subPath == "" {
			return "", true, nil
		}
		return "", false, nil
	}
	rel := strings.Join(segs[1:], "/")
	if subPath == "" {
		return rel, true, nil
	}
	if rel == subPath {
		return "", true, nil
	}
	if strings.HasPrefix(rel, subPath+"/") {
		return strings.TrimPrefix(rel, subPath+"/"), true, nil
	}
	return "", false, nil
}

func normalizeArchiveMode(archiveMode int64) os.FileMode {
	if archiveMode&0o111 != 0 {
		return 0o755
	}
	return 0o644
}
