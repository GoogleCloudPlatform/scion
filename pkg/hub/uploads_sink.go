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

package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	uploadPartialPrefix = ".scion-upload-"
	uploadPartialSuffix = ".partial"
	uploadCopyBufSize   = 64 * 1024
)

var (
	// errUploadIntegrityMismatch is returned by FileSink.Finalize when the
	// partial file size does not match DeclaredSize or when takeovers > 0 and
	// re-hashing the partial file does not match the committed SHA-256 digest
	// (§3.6).
	errUploadIntegrityMismatch = errors.New("upload integrity mismatch")

	// errUploadTargetExists is returned by FileSink.Finalize when the target
	// file already exists and overwrite is false (§3.6).
	errUploadTargetExists = errors.New("upload target already exists")

	// errUploadShortBody is returned when the chunk request body ends before
	// delivering the declared byte count.
	errUploadShortBody = errors.New("upload chunk body shorter than Content-Length")

	// errUploadChunkTooLarge is returned when the chunk request body exceeds
	// the declared byte count.
	errUploadChunkTooLarge = errors.New("upload chunk body exceeds declared length")

	// errUploadPartialMissing is returned by FileSink.Finalize when the partial
	// file is missing and the target does not already hold the finalized file.
	errUploadPartialMissing = errors.New("upload partial file missing")
)

// uploadPartialName returns the staging filename for session sid inside the
// target file's parent directory (§3.6).
func uploadPartialName(sid string) string {
	return uploadPartialPrefix + sid + uploadPartialSuffix
}

// isUploadPartialName reports whether name is a staging partial file created by
// FileSink. Workspace and shared-dir file listings hide these entries (§3.6).
func isUploadPartialName(name string) bool {
	return strings.HasPrefix(name, uploadPartialPrefix) && strings.HasSuffix(name, uploadPartialSuffix)
}

// uploadStagingRelPath returns the relative path within the target root for
// sess's FileSink partial file.
func uploadStagingRelPath(destPath, sid string) string {
	dir := path.Dir(destPath)
	partial := uploadPartialName(sid)
	if dir == "." || dir == "" || dir == "/" {
		return partial
	}
	return path.Join(dir, partial)
}

// uploadSink abstracts staging, finalization, and cleanup of resumable chunked
// upload bytes (ptone/scion#8654 §3.6). FileSink implements it for workspace
// (P1b) and shared-dir (P3) targets; ObjectSink implements it for chat
// attachments (P2).
type uploadSink interface {
	// WriteChunk writes exactly n bytes from r at byte offset into staging,
	// checking ctx between writes. Returns an optional part reference
	// (empty for FileSink; staged object name for ObjectSink).
	WriteChunk(ctx context.Context, sess *store.UploadSession, offset int64, token string, r io.Reader, n int64) (partRef string, err error)

	// Finalize promotes staged bytes to the target destination and returns the
	// JSON-encoded completion response (stored on the session row for replay).
	// digestHex is the lowercase hex SHA-256 of the uploaded bytes.
	// Must be idempotent across retries after an expired finalize lease.
	Finalize(ctx context.Context, sess *store.UploadSession, digestHex string) (json.RawMessage, error)

	// Cleanup removes any staged bytes for sess. Must be idempotent and return
	// nil if staging is already absent.
	Cleanup(ctx context.Context, sess *store.UploadSession) error
}

// fileUploadSink implements uploadSink by staging bytes into a sibling
// .scion-upload-{sid}.partial file inside the target's parent directory,
// confined through *os.Root and openWorkspaceWriteParent (§3.6).
type fileUploadSink struct {
	root *os.Root
}

var _ uploadSink = (*fileUploadSink)(nil)

func newFileUploadSink(root *os.Root) *fileUploadSink {
	return &fileUploadSink{root: root}
}

// Prepare creates any missing parent directories for destPath under the same
// confinement rules as writeWorkspaceFile, and refuses a symlink or
// non-regular entry already at destPath.
func (s *fileUploadSink) Prepare(destPath string) error {
	parent, destBase, err := openWorkspaceWriteParent(s.root, destPath)
	if err != nil {
		return err
	}
	defer parent.Close()
	if info, err := parent.Dir().Lstat(destBase); err == nil {
		if notRegular(info) {
			return fmt.Errorf("%w: target is not a regular file", errWorkspaceWriteRefused)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// WriteChunk streams exactly n bytes into .scion-upload-{sid}.partial at
// offset, checking ctx between 64 KiB writes, then syncs and closes the file.
func (s *fileUploadSink) WriteChunk(ctx context.Context, sess *store.UploadSession, offset int64, _ string, r io.Reader, n int64) (string, error) {
	parent, _, err := openWorkspaceWriteParent(s.root, sess.DestPath)
	if err != nil {
		return "", err
	}
	defer parent.Close()
	dir := parent.Dir()
	partialBase := uploadPartialName(sess.ID)

	var (
		f            *os.File
		before       fs.FileInfo
		createdFresh bool
	)

	if offset == 0 {
		// Create exclusively at offset 0 so a pre-planted symlink or non-regular
		// entry is refused. If the partial already exists (e.g. a retry after an
		// earlier offset-0 commit failure or lease takeover), accept it only if
		// it is a regular file under the same SameFile checks as offset > 0,
		// then truncate to 0.
		f, err = openWorkspaceFileForWrite(dir, partialBase, false)
		if err == nil {
			createdFresh = true
		} else {
			linfo, lerr := dir.Lstat(partialBase)
			if lerr != nil {
				return "", err
			}
			if notRegular(linfo) {
				return "", fmt.Errorf("%w: partial is not a regular file", errWorkspaceWriteRefused)
			}
			before = linfo
			f, err = openWorkspaceFileForWrite(dir, partialBase, true)
			if err != nil {
				return "", err
			}
		}
	} else {
		before, err = dir.Lstat(partialBase)
		if err != nil {
			return "", err
		}
		if notRegular(before) {
			return "", fmt.Errorf("%w: partial is not a regular file", errWorkspaceWriteRefused)
		}
		f, err = openWorkspaceFileForWrite(dir, partialBase, true)
		if err != nil {
			return "", err
		}
	}

	var opened fs.FileInfo
	cleanupOnErr := func() {
		if createdFresh && opened != nil {
			if cur, lerr := dir.Lstat(partialBase); lerr == nil && os.SameFile(opened, cur) {
				_ = dir.Remove(partialBase)
			}
		}
		_ = f.Close()
	}

	// Confirm the opened handle is a regular file matching the directory entry.
	opened, err = f.Stat()
	if err != nil {
		cleanupOnErr()
		return "", err
	}
	named, err := dir.Lstat(partialBase)
	if err != nil {
		cleanupOnErr()
		return "", err
	}
	if notRegular(opened) || notRegular(named) || !os.SameFile(opened, named) || (before != nil && !os.SameFile(opened, before)) {
		cleanupOnErr()
		return "", fmt.Errorf("%w: partial changed while opening it", errWorkspaceWriteRefused)
	}

	if err := f.Truncate(offset); err != nil {
		cleanupOnErr()
		return "", err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		cleanupOnErr()
		return "", err
	}

	buf := make([]byte, uploadCopyBufSize)
	var written int64
	for {
		if err := ctx.Err(); err != nil {
			cleanupOnErr()
			return "", err
		}
		nr, rerr := r.Read(buf)
		if nr > 0 {
			if written+int64(nr) > n {
				cleanupOnErr()
				return "", errUploadChunkTooLarge
			}
			if err := ctx.Err(); err != nil {
				cleanupOnErr()
				return "", err
			}
			nw, werr := f.Write(buf[:nr])
			written += int64(nw)
			if werr != nil {
				cleanupOnErr()
				return "", werr
			}
			if nw != nr {
				cleanupOnErr()
				return "", io.ErrShortWrite
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			cleanupOnErr()
			return "", rerr
		}
	}
	if written != n {
		cleanupOnErr()
		return "", errUploadShortBody
	}

	if err := f.Sync(); err != nil {
		cleanupOnErr()
		return "", err
	}
	if err := f.Close(); err != nil {
		if createdFresh && opened != nil {
			if cur, lerr := dir.Lstat(partialBase); lerr == nil && os.SameFile(opened, cur) {
				_ = dir.Remove(partialBase)
			}
		}
		return "", err
	}
	return "", nil
}

// Finalize verifies integrity (re-hashing when takeovers > 0), checks target
// overwrite and regular-file constraints, atomically renames the partial file
// into destPath through *os.Root, and returns the ProjectWorkspaceFile JSON.
func (s *fileUploadSink) Finalize(ctx context.Context, sess *store.UploadSession, digestHex string) (json.RawMessage, error) {
	parent, destBase, err := openWorkspaceWriteParent(s.root, sess.DestPath)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	dir := parent.Dir()
	partialBase := uploadPartialName(sess.ID)

	pInfo, err := dir.Lstat(partialBase)
	if errors.Is(err, fs.ErrNotExist) {
		// Zero-byte uploads never wrote a chunk; create an empty partial now.
		if sess.DeclaredSize == 0 {
			if err := checkFinalizeDest(dir, destBase, sess.Overwrite); err != nil {
				return nil, err
			}
			f, cerr := openWorkspaceFileForWrite(dir, partialBase, false)
			if cerr != nil {
				return nil, cerr
			}
			if serr := f.Sync(); serr != nil {
				_ = f.Close()
				_ = dir.Remove(partialBase)
				return nil, serr
			}
			if cerr := f.Close(); cerr != nil {
				_ = dir.Remove(partialBase)
				return nil, cerr
			}
			if rerr := dir.Rename(partialBase, destBase); rerr != nil {
				_ = dir.Remove(partialBase)
				return nil, rerr
			}
			return marshalWorkspaceFileResult(dir, destBase, sess.DestPath)
		}

		// Idempotent finalize retry (§3.7): if the partial is already gone and
		// dest is present as a regular file with matching size, treat as done.
		if dInfo, derr := dir.Lstat(destBase); derr == nil && !notRegular(dInfo) && dInfo.Size() == sess.DeclaredSize {
			return marshalWorkspaceFileInfo(sess.DestPath, dInfo)
		}
		return nil, errUploadPartialMissing
	}
	if err != nil {
		return nil, err
	}
	if notRegular(pInfo) {
		return nil, fmt.Errorf("%w: partial is not a regular file", errWorkspaceWriteRefused)
	}
	if pInfo.Size() != sess.DeclaredSize {
		return nil, fmt.Errorf("%w: partial size %d does not match declared size %d", errUploadIntegrityMismatch, pInfo.Size(), sess.DeclaredSize)
	}

	// Fencing backstop (§3.6): if a lease takeover occurred, re-hash the
	// partial file and confirm it matches the committed SHA-256 digest.
	if sess.Takeovers > 0 {
		if err := verifyPartialHash(ctx, dir, partialBase, pInfo, digestHex); err != nil {
			return nil, err
		}
	}

	if err := checkFinalizeDest(dir, destBase, sess.Overwrite); err != nil {
		return nil, err
	}

	if err := dir.Rename(partialBase, destBase); err != nil {
		return nil, err
	}

	return marshalWorkspaceFileResult(dir, destBase, sess.DestPath)
}

// Cleanup removes the partial file through *os.Root without creating any
// missing directories. Returns nil when the parent or partial is already gone.
func (s *fileUploadSink) Cleanup(_ context.Context, sess *store.UploadSession) error {
	parent, _, err := openWorkspaceParent(s.root, sess.DestPath, false, true)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer parent.Close()
	partialBase := uploadPartialName(sess.ID)
	if err := parent.Dir().Remove(partialBase); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func checkFinalizeDest(dir *os.Root, destBase string, overwrite bool) error {
	dInfo, err := dir.Lstat(destBase)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if notRegular(dInfo) {
		return fmt.Errorf("%w: target is not a regular file", errWorkspaceWriteRefused)
	}
	if !overwrite {
		return errUploadTargetExists
	}
	return nil
}

func verifyPartialHash(ctx context.Context, dir *os.Root, partialBase string, expected fs.FileInfo, wantHex string) error {
	f, _, err := openWorkspaceRegularForRead(dir, partialBase, expected)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	buf := make([]byte, uploadCopyBufSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		nr, rerr := f.Read(buf)
		if nr > 0 {
			_, _ = h.Write(buf[:nr])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	gotHex := hex.EncodeToString(h.Sum(nil))
	if gotHex != wantHex {
		return fmt.Errorf("%w: partial digest %s does not match committed digest %s", errUploadIntegrityMismatch, gotHex, wantHex)
	}
	return nil
}

func marshalWorkspaceFileResult(dir *os.Root, destBase, destPath string) (json.RawMessage, error) {
	info, err := dir.Stat(destBase)
	if err != nil {
		return nil, err
	}
	return marshalWorkspaceFileInfo(destPath, info)
}

func marshalWorkspaceFileInfo(destPath string, info fs.FileInfo) (json.RawMessage, error) {
	entry := ProjectWorkspaceFile{
		Path:    destPath,
		Size:    info.Size(),
		ModTime: info.ModTime(),
		Mode:    info.Mode().String(),
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}
