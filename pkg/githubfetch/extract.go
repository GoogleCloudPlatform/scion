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
}

var errUnpackedLimit = errors.New("unpacked archive stream exceeds limit")

type unpackedCapReader struct {
	r         io.Reader
	remaining int64
}

func (c *unpackedCapReader) Read(p []byte) (int, error) {
	if c.remaining < 0 {
		return 0, errUnpackedLimit
	}
	if int64(len(p)) > c.remaining+1 {
		p = p[:c.remaining+1]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	if c.remaining < 0 {
		return n, errUnpackedLimit
	}
	return n, err
}

// extractTarGz streams a GitHub gzip tarball in a single pass, keeping only
// entries under subPath, writing them through dst (*os.Root) with O_EXCL and
// normalized 0644/0755 modes, hashing each written file's git blob SHA on the
// fly, and verifying the PAX global commit SHA and (when tree is non-nil and
// not truncated) the exact file set and blob SHAs against tree.
func extractTarGz(r io.Reader, repo, subPath, expectedSHA string, tree *Tree, dst *os.Root, l Limits) (int, int64, SkipCounts, error) {
	l = l.withDefaults()
	gzr, err := gzip.NewReader(r)
	if err != nil {
		var ghe *Error
		if errors.As(err, &ghe) {
			return 0, 0, SkipCounts{}, ghe
		}
		return 0, 0, SkipCounts{}, &Error{Kind: KindUpstream, Repo: repo, Hint: "invalid gzip archive", err: err}
	}
	defer func() { _ = gzr.Close() }()

	var tarStream io.Reader = gzr
	if l.MaxUnpacked > 0 {
		tarStream = &unpackedCapReader{r: gzr, remaining: l.MaxUnpacked}
	}
	tr := tar.NewReader(tarStream)

	subPath = strings.Trim(subPath, "/")
	var (
		written     int64
		entries     int
		found       bool
		paxSHA      string
		skipped     SkipCounts
		actualBlobs = make(map[string]string)
	)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, errUnpackedLimit) {
				return 0, 0, skipped, &Error{Kind: KindTooLarge, Repo: repo, Hint: "decompressed archive exceeds MaxUnpacked"}
			}
			var ghe *Error
			if errors.As(err, &ghe) {
				return 0, 0, skipped, ghe
			}
			return 0, 0, skipped, ClassifyRemoteFetchError(err, repo, Credential{}, nowUTC())
		}

		if hdr.Typeflag == tar.TypeXGlobalHeader {
			if paxSHA == "" {
				cand := strings.TrimSpace(hdr.PAXRecords["comment"])
				if IsFullCommitSHA(cand) {
					paxSHA = cand
				}
			}
			continue
		}

		// Devices and FIFOs are always refused.
		switch hdr.Typeflag {
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return 0, 0, skipped, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "archive contains a device or FIFO entry"}
		}

		// Validate raw tar entry path before stripping the top-level directory.
		rel, inSub, err := stripTarPath(hdr.Name, subPath, repo)
		if err != nil {
			return 0, 0, skipped, err
		}
		if !inSub {
			continue
		}

		if rel != "" && hasDotGitSegment(rel) {
			skipped.DotGit++
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeSymlink:
			if l.OnSymlink == PolicyReject {
				return 0, 0, skipped, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "archive contains a symlink"}
			}
			skipped.Symlinks++
			continue
		case tar.TypeLink:
			skipped.Hardlinks++
			continue
		case tar.TypeDir, tar.TypeReg:
			// Handled below.
		default:
			continue
		}

		found = true
		if rel == "" {
			continue
		}
		if len(rel) > l.MaxNameBytes {
			return 0, 0, skipped, &Error{Kind: KindTooLarge, Repo: repo, Hint: "archive entry path exceeds MaxNameBytes"}
		}

		entries++
		if l.MaxFiles > 0 && entries > l.MaxFiles {
			return 0, 0, skipped, &Error{Kind: KindTooLarge, Repo: repo, Hint: "archive exceeds MaxFiles"}
		}

		if hdr.Typeflag == tar.TypeDir {
			d, err := rootopen.MkdirAll(dst, rel, 0o755)
			if err != nil {
				return 0, 0, skipped, err
			}
			_ = d.Close()
			continue
		}

		// Regular file.
		if hdr.Size < 0 || (l.MaxFileBytes > 0 && hdr.Size > l.MaxFileBytes) {
			return 0, 0, skipped, &Error{Kind: KindTooLarge, Repo: repo, Hint: "archive file exceeds MaxFileBytes"}
		}
		if l.MaxTotal > 0 && written+hdr.Size > l.MaxTotal {
			return 0, 0, skipped, &Error{Kind: KindTooLarge, Repo: repo, Hint: "extracted files exceed MaxTotal"}
		}

		if dir := path.Dir(rel); dir != "." && dir != "" {
			d, err := rootopen.MkdirAll(dst, dir, 0o755)
			if err != nil {
				return 0, 0, skipped, err
			}
			_ = d.Close()
		}

		mode := normalizeArchiveMode(hdr.Mode)
		f, err := rootopen.OpenFileForWrite(dst, rel, uint32(mode), false)
		if err != nil {
			return 0, 0, skipped, fmt.Errorf("create extracted file %q: %w", rel, err)
		}
		// Apply the normalized mode independent of process umask.
		if err := f.Chmod(mode); err != nil { //winsec:not-protection normalize extracted file mode to 0644/0755
			_ = f.Close()
			return 0, 0, skipped, err
		}

		h := sha1.New() //nolint:gosec // git blob object IDs are SHA-1
		_, _ = fmt.Fprintf(h, "blob %d\x00", hdr.Size)

		var src io.Reader = tr
		if l.MaxTotal > 0 {
			src = io.LimitReader(tr, l.MaxTotal-written+1)
		}
		n, copyErr := io.Copy(io.MultiWriter(f, h), src)
		closeErr := f.Close()
		written += n
		if copyErr != nil {
			if errors.Is(copyErr, errUnpackedLimit) {
				return 0, 0, skipped, &Error{Kind: KindTooLarge, Repo: repo, Hint: "decompressed archive exceeds MaxUnpacked"}
			}
			var ghe *Error
			if errors.As(copyErr, &ghe) {
				return 0, 0, skipped, ghe
			}
			return 0, 0, skipped, ClassifyRemoteFetchError(copyErr, repo, Credential{}, nowUTC())
		}
		if l.MaxTotal > 0 && written > l.MaxTotal {
			return 0, 0, skipped, &Error{Kind: KindTooLarge, Repo: repo, Hint: "extracted files exceed MaxTotal"}
		}
		if closeErr != nil {
			return 0, 0, skipped, closeErr
		}
		actualBlobs[rel] = hex.EncodeToString(h.Sum(nil))
	}

	if !found {
		return 0, 0, skipped, &Error{Kind: KindNotFound, Repo: repo, Hint: "source folder not found in archive"}
	}

	if expectedSHA != "" && paxSHA != expectedSHA {
		return 0, 0, skipped, &Error{Kind: KindUpstream, Repo: repo, Hint: "archive PAX commit SHA does not match resolved commit"}
	}

	// Verify archive ⊆ tree: every extracted regular file must appear in the
	// git tree with a matching git blob SHA. Tree files absent from the archive
	// (e.g. omitted by .gitattributes export-ignore) are counted in
	// skipped.ExportIgnored rather than failing.
	//
	// Known limitation: files rewritten by git archive (such as .gitattributes
	// export-subst or LFS-content-in-archive) have different bytes than the git
	// blob SHA in the tree and fail integrity verification.
	if tree != nil && !tree.Truncated {
		expected := make(map[string]File, len(tree.Files))
		for _, tf := range tree.Files {
			expected[tf.Path] = tf
		}
		for rel, gotSHA := range actualBlobs {
			tf, ok := expected[rel]
			if !ok {
				return 0, 0, skipped, &Error{Kind: KindUpstream, Repo: repo, Hint: "archive contains file not listed in git tree"}
			}
			if tf.BlobSHA != "" && gotSHA != tf.BlobSHA {
				return 0, 0, skipped, &Error{Kind: KindUpstream, Repo: repo, Hint: "archive file blob SHA mismatch"}
			}
		}
		for _, tf := range tree.Files {
			if _, ok := actualBlobs[tf.Path]; !ok {
				skipped.ExportIgnored++
			}
		}
	}

	return len(actualBlobs), written, skipped, nil
}

// stripTarPath validates rawName (rejecting absolute paths, backslashes, NUL
// bytes, "." and ".." segments, and empty interior segments), strips the
// archive's single top-level directory, and returns the path relative to
// subPath and whether the entry falls inside subPath.
func stripTarPath(rawName, subPath, repo string) (string, bool, error) {
	if rawName == "" || strings.ContainsAny(rawName, "\\\x00") {
		return "", false, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "unsafe archive entry path"}
	}
	if strings.HasPrefix(rawName, "/") {
		return "", false, &Error{Kind: KindUnsupportedContent, Repo: repo, Hint: "archive contains absolute path"}
	}
	trimmed := strings.TrimSuffix(rawName, "/")
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
