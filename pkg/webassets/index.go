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

// Package webassets serves the web client's static files with precompressed
// variants (build-time *.br and *.gz siblings) and validators.
//
// The build writes file.br and file.gz next to each compressible file. At
// startup NewIndex walks the embedded file system once, hashes every
// servable file and pairs it with its siblings. Serve then negotiates the
// coding from Accept-Encoding, sets Vary, Content-Type (always from the raw
// name), Content-Encoding, ETag and Cache-Control, and hands the selected
// representation to http.ServeContent, which handles If-None-Match (304),
// Range and HEAD. For a directory that can change while the hub runs
// (--web-assets-dir), DiskIndex does the same per request with a weak,
// size-and-mtime validator.
//
// The package does not depend on pkg/hub; the hub falls back to its own
// file server whenever Serve reports found=false.
package webassets

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// CachePolicy returns the Cache-Control value for a request path.
type CachePolicy func(requestPath string) string

// Index serves files from a file system that does not change while the hub
// runs (the embedded client build). It is immutable after NewIndex and safe
// for concurrent use.
type Index struct {
	fsys    fs.FS
	entries map[string]*entry // key: slash path relative to fsys root, e.g. "assets/main.js"
}

type entry struct {
	etag        string              // strong validator of the raw file, with quotes
	contentType string              // from the raw name (or sniffed from the raw bytes)
	variants    map[Encoding]string // encoded variant -> file name in fsys
}

// NewIndex walks fsys, hashes the servable files and pairs each with its
// .br/.gz siblings. Servable files are the root-level files and the files
// under assets/ and shoelace/. Files named *.br or *.gz are never servable
// by their own name.
func NewIndex(fsys fs.FS) (*Index, error) {
	if fsys == nil {
		return nil, fmt.Errorf("webassets: NewIndex: nil file system")
	}
	ix := &Index{fsys: fsys, entries: map[string]*entry{}}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name != "." && !inScope(name+"/x") {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || IsVariantName(name) || !inScope(name) {
			return nil
		}
		e, err := newEntry(fsys, name)
		if err != nil {
			return fmt.Errorf("webassets: index %s: %w", name, err)
		}
		ix.entries[name] = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ix, nil
}

func newEntry(fsys fs.FS, name string) (*entry, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	head := &prefixWriter{max: 512}
	if _, err := io.Copy(io.MultiWriter(h, head), f); err != nil {
		return nil, err
	}
	sum := h.Sum(nil)
	e := &entry{
		etag:        `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`,
		contentType: contentTypeFor(name, head.buf.Bytes()),
		variants:    map[Encoding]string{},
	}
	for enc, suffix := range variantSuffix {
		if info, err := fs.Stat(fsys, name+suffix); err == nil && info.Mode().IsRegular() {
			e.variants[enc] = name + suffix
		}
	}
	return e, nil
}

// Serve writes the file for r.URL.Path, or returns false without writing
// anything when the path is not an indexed file (a directory, a missing or
// unindexed file, a *.br/*.gz name, a malformed path, or a method other than
// GET and HEAD), so the caller can fall back to its own handler.
func (ix *Index) Serve(w http.ResponseWriter, r *http.Request, p CachePolicy) bool {
	if ix == nil || !servableMethod(r.Method) {
		return false
	}
	name, ok := lookupName(r.URL.Path)
	if !ok {
		return false
	}
	e, ok := ix.entries[name]
	if !ok {
		return false
	}
	available := map[Encoding]bool{}
	for enc := range e.variants {
		available[enc] = true
	}
	enc := Negotiate(r.Header.Values("Accept-Encoding"), available)
	file := name
	etag := e.etag
	if enc != Identity {
		file = e.variants[enc]
		etag = strings.TrimSuffix(e.etag, `"`) + etagSuffix[enc] + `"`
	}
	f, err := ix.fsys.Open(file)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	rs, err := readSeeker(f)
	if err != nil {
		return false
	}
	writeHeaders(w, r, p, e.contentType, len(e.variants) > 0, enc, etag)
	http.ServeContent(w, r, "", time.Time{}, rs)
	return true
}

// Len reports the number of servable files in the index.
func (ix *Index) Len() int {
	if ix == nil {
		return 0
	}
	return len(ix.entries)
}

// writeHeaders sets the representation headers shared by Index and
// DiskIndex. A Content-Type the caller already set wins (the hub's own
// overrides); otherwise the raw name's type is used, never the type of a
// .br/.gz name.
func writeHeaders(w http.ResponseWriter, r *http.Request, p CachePolicy, contentType string, hasVariants bool, enc Encoding, etag string) {
	h := w.Header()
	if hasVariants {
		h.Add("Vary", "Accept-Encoding")
	}
	if h.Get("Content-Type") == "" && contentType != "" {
		h.Set("Content-Type", contentType)
	}
	if enc != Identity {
		h.Set("Content-Encoding", string(enc))
	}
	h.Set("ETag", etag)
	if p != nil {
		if cc := p(r.URL.Path); cc != "" {
			h.Set("Cache-Control", cc)
		}
	}
}

// servableMethod reports whether the method is one Serve answers (GET and
// HEAD). Other methods are left to the caller's fallback, which never
// serves a variant.
func servableMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// inScope reports whether a slash path relative to the asset root is one
// the index serves: a root-level file or a file under assets/ or shoelace/.
func inScope(name string) bool {
	if !strings.Contains(name, "/") {
		return true
	}
	return strings.HasPrefix(name, "assets/") || strings.HasPrefix(name, "shoelace/")
}

// lookupName maps a URL path to a clean slash path relative to the asset
// root. It rejects anything that is not already clean and inside the root:
// "..", ".", empty or doubled segments, backslashes, NUL bytes, variant
// names and index.html.
func lookupName(urlPath string) (string, bool) {
	if !strings.HasPrefix(urlPath, "/") {
		return "", false
	}
	name := urlPath[1:]
	if name == "" || strings.ContainsAny(name, "\\\x00") || !fs.ValidPath(name) || path.Clean(name) != name {
		return "", false
	}
	if IsVariantName(name) || !inScope(name) {
		return "", false
	}
	// http.FileServer redirects .../index.html to the directory; leave such
	// names to the caller's fallback so that behaviour is kept.
	if path.Base(name) == "index.html" {
		return "", false
	}
	return name, true
}

// contentTypeFor derives the Content-Type from the raw file name, falling
// back to sniffing the raw bytes (as http.ServeContent would).
func contentTypeFor(name string, head []byte) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return http.DetectContentType(head)
}

// readSeeker returns f as an io.ReadSeeker, buffering it when the file
// system's files cannot seek.
func readSeeker(f fs.File) (io.ReadSeeker, error) {
	if rs, ok := f.(io.ReadSeeker); ok {
		return rs, nil
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(b), nil
}

// prefixWriter keeps the first max bytes written to it.
type prefixWriter struct {
	buf bytes.Buffer
	max int
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	if room := p.max - p.buf.Len(); room > 0 {
		if len(b) < room {
			room = len(b)
		}
		p.buf.Write(b[:room])
	}
	return len(b), nil
}
