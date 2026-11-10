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

package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
)

// ReadCollectedFile returns the content of fi, an entry returned by
// CollectFiles, read through root, which must be opened at the directory
// that was collected. Reading through os.Root refuses paths and links that
// leave that directory; links that stay inside it are resolved. The entry
// itself must still be a regular file (not a link), and the content read
// must match the recorded size and hash, so the bytes returned are exactly
// the ones described by the collected manifest entry. Any mismatch is an
// error and no content is returned.
func ReadCollectedFile(root *os.Root, fi FileInfo) ([]byte, error) {
	rc, err := OpenCollectedFile(root, fi)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// OpenCollectedFile opens fi, an entry returned by CollectFiles, through
// root, which must be opened at the directory that was collected, and
// returns a reader of its content. It applies the checks of
// ReadCollectedFile without holding the whole file in memory: the entry
// must still be a regular file of the recorded size when it is opened, and
// the content is hashed while it is read. If the content read differs from
// the recorded size or hash, the read that reaches the end returns an error
// instead of io.EOF, so a consumer that reads to the end fails rather than
// accept changed content. The caller must close the reader.
func OpenCollectedFile(root *os.Root, fi FileInfo) (io.ReadCloser, error) {
	rel := filepath.FromSlash(fi.Path)

	// The entry itself must still be a regular file (OpenRegularFile: Lstat,
	// a non-blocking open, so a FIFO swapped in after the Lstat does not
	// block, then fstat and SameFile on the opened file).
	f, info, err := OpenRegularFile(root, rel)
	switch {
	case errors.Is(err, ErrNotRegularFile):
		return nil, fmt.Errorf("%s is no longer a regular file", fi.Path)
	case errors.Is(err, ErrFileReplaced):
		return nil, fmt.Errorf("%s changed since it was collected (replaced while it was opened)", fi.Path)
	case err != nil:
		return nil, fmt.Errorf("failed to read %s: %w", fi.Path, err)
	}
	if info.Size() != fi.Size {
		_ = f.Close()
		return nil, fmt.Errorf("%s changed since it was collected (size %d, collected %d)", fi.Path, info.Size(), fi.Size)
	}

	return &collectedReader{
		f:  f,
		r:  io.LimitReader(f, fi.Size+1), // one byte more than recorded detects growth
		h:  sha256.New(),
		fi: fi,
	}, nil
}

// collectedReader reads a collected file and verifies its size and hash
// when the end is reached.
type collectedReader struct {
	f  *os.File
	r  io.Reader
	h  hash.Hash
	n  int64
	fi FileInfo
}

func (c *collectedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.h.Write(p[:n])
	c.n += int64(n)
	if c.n > c.fi.Size {
		return n, fmt.Errorf("%s changed since it was collected (read more than the collected %d bytes)", c.fi.Path, c.fi.Size)
	}
	if err == io.EOF {
		if c.n != c.fi.Size {
			return n, fmt.Errorf("%s changed since it was collected (read %d bytes, collected %d)", c.fi.Path, c.n, c.fi.Size)
		}
		if HashPrefix+hex.EncodeToString(c.h.Sum(nil)) != c.fi.Hash {
			return n, fmt.Errorf("%s changed since it was collected (content hash differs)", c.fi.Path)
		}
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("failed to read %s: %w", c.fi.Path, err)
	}
	return n, nil
}

func (c *collectedReader) Close() error {
	return c.f.Close()
}
