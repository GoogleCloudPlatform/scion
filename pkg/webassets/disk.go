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
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
)

// DiskIndex serves files from a directory that may change while the hub
// runs (--web-assets-dir). Nothing is cached: every request stats the file
// and its siblings. The validator is weak, W/"<size>-<mtimeUnixNano>" of the
// representation sent (with the -br/-gz suffix for an encoded variant),
// because hashing on every request would cost too much. A .br/.gz sibling
// is used only if its mtime is not older than the source file's, so a
// stale sibling left next to a rebuilt file is ignored.
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
// anything when the path is not a servable regular file (see Index.Serve).
func (d *DiskIndex) Serve(w http.ResponseWriter, r *http.Request, p CachePolicy) bool {
	if d == nil || d.dir == "" {
		return false
	}
	name, ok := lookupName(r.URL.Path)
	if !ok {
		return false
	}
	rawPath := filepath.Join(d.dir, filepath.FromSlash(name))
	raw, err := os.Stat(rawPath)
	if err != nil || !raw.Mode().IsRegular() {
		return false
	}

	available := map[Encoding]bool{}
	variantInfo := map[Encoding]os.FileInfo{}
	for enc, suffix := range variantSuffix {
		info, err := os.Stat(rawPath + suffix)
		if err != nil || !info.Mode().IsRegular() || info.ModTime().Before(raw.ModTime()) {
			continue
		}
		available[enc] = true
		variantInfo[enc] = info
	}
	enc := Negotiate(r.Header.Values("Accept-Encoding"), available)
	file, info, suffix := rawPath, raw, ""
	if enc != Identity {
		file, info, suffix = rawPath+variantSuffix[enc], variantInfo[enc], etagSuffix[enc]
	}

	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()

	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = sniffFile(rawPath)
	}
	etag := fmt.Sprintf(`W/"%d-%d%s"`, info.Size(), info.ModTime().UnixNano(), suffix)
	writeHeaders(w, r, p, contentType, len(available) > 0, enc, etag)
	http.ServeContent(w, r, "", raw.ModTime(), f)
	return true
}

// sniffFile sniffs the Content-Type of the first 512 bytes of a file.
func sniffFile(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	return http.DetectContentType(head[:n])
}
