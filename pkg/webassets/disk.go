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

package webassets

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
)

// DiskIndex serves files from a directory that may change while the hub
// runs (--web-assets-dir). Nothing is cached: every request stats the file
// and its siblings. The validator is weak, W/"<size>-<mtimeUnixNano>" of the
// representation sent (with the -br/-gz suffix for an encoded variant),
// because hashing on every request would cost too much. A .br/.gz sibling
// is used only if its mtime is not older than the source file's, so a
// stale sibling left next to a rebuilt file is ignored.
//
// Files are opened through os.Root, so neither ".." nor a symlink can reach
// outside the directory.
type DiskIndex struct {
	dir string
}

// NewDiskIndex returns a DiskIndex for dir. It does not touch the disk.
func NewDiskIndex(dir string) *DiskIndex {
	return &DiskIndex{dir: dir}
}

// Dir returns the directory the index serves.
func (d *DiskIndex) Dir() string {
	if d == nil {
		return ""
	}
	return d.dir
}

// Serve writes the file for r.URL.Path, or returns false without writing
// anything when the path is not a servable regular file inside the
// directory (see Index.Serve).
func (d *DiskIndex) Serve(w http.ResponseWriter, r *http.Request, p CachePolicy) bool {
	if d == nil || d.dir == "" || !servableMethod(r.Method) {
		return false
	}
	name, ok := lookupName(r.URL.Path)
	if !ok {
		return false
	}
	// The root is opened per request, so a directory that is replaced while
	// the hub runs is picked up.
	root, err := os.OpenRoot(d.dir)
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()

	raw, err := root.Stat(name)
	if err != nil || !raw.Mode().IsRegular() {
		return false
	}
	available := map[Encoding]bool{}
	variantInfo := map[Encoding]fs.FileInfo{}
	for enc, suffix := range variantSuffix {
		info, err := root.Stat(name + suffix)
		if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(raw.ModTime()) {
			continue
		}
		available[enc] = true
		variantInfo[enc] = info
	}
	enc := Negotiate(r.Header.Values("Accept-Encoding"), available)
	file, suffix := name, ""
	if enc != Identity {
		file, suffix = name+variantSuffix[enc], etagSuffix[enc]
	}

	f, err := root.Open(file)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	// Use the opened file's own metadata for the validator, so the ETag
	// describes exactly the bytes sent.
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}

	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = sniff(root, name)
	}
	etag := fmt.Sprintf(`W/"%d-%d%s"`, info.Size(), info.ModTime().UnixNano(), suffix)
	writeHeaders(w, r, p, contentType, len(available) > 0, enc, etag)
	http.ServeContent(w, r, "", raw.ModTime(), f)
	return true
}

// sniff sniffs the Content-Type of the first 512 bytes of a file.
func sniff(root *os.Root, name string) string {
	f, err := root.Open(name)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	return http.DetectContentType(head[:n])
}

// DirFileSystem returns an http.FileSystem over dir for a fallback
// http.FileServer. Like http.Dir it serves files and directory listings,
// but it opens through os.Root: a path that leaves the directory, by ".."
// or through a symlink, reports fs.ErrNotExist (a 404), and a precompressed
// sibling (*.br, *.gz) is never opened by its own name.
func DirFileSystem(dir string) http.FileSystem {
	return dirFileSystem{dir: dir}
}

type dirFileSystem struct {
	dir string
}

func (d dirFileSystem) Open(name string) (http.File, error) {
	if IsVariantName(name) {
		return nil, fs.ErrNotExist
	}
	root, err := os.OpenRoot(d.dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	rel := path.Clean("/" + name)[1:]
	if rel == "" {
		rel = "."
	}
	f, err := root.Open(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
			return nil, err
		}
		// Names that resolve outside the root, and malformed names, are reported as missing.
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return f, nil
}

// FSFileSystem returns an http.FileSystem over fsys for a fallback
// http.FileServer. It is http.FS(fsys), except that a precompressed sibling
// (*.br, *.gz) is never opened by its own name.
func FSFileSystem(fsys fs.FS) http.FileSystem {
	return variantRefusingFS{http.FS(fsys)}
}

type variantRefusingFS struct {
	http.FileSystem
}

func (v variantRefusingFS) Open(name string) (http.File, error) {
	if IsVariantName(name) {
		return nil, fs.ErrNotExist
	}
	return v.FileSystem.Open(name)
}
