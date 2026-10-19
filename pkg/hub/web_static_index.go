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
	"io/fs"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/GoogleCloudPlatform/scion/pkg/webassets"
	"github.com/GoogleCloudPlatform/scion/web"
)

// embeddedClientFS is the embedded client build. It is created once per
// process, so every WebServer shares one value and one index of it
// (embeddedIndex): the embedded files never change, and hashing them again
// for each NewWebServer would only cost time.
var embeddedClientFS = sync.OnceValues(func() (fs.FS, error) {
	return fs.Sub(web.ClientAssets, "dist/client")
})

var embeddedIndex = sync.OnceValues(func() (*webassets.Index, error) {
	sub, err := embeddedClientFS()
	if err != nil {
		return nil, err
	}
	return webassets.NewIndex(sub)
})

// newIndexFor returns the index for fsys, reusing the process-wide index
// of the embedded build.
func newIndexFor(fsys fs.FS) (*webassets.Index, error) {
	if sub, err := embeddedClientFS(); err == nil && sameFS(fsys, sub) {
		return embeddedIndex()
	}
	return webassets.NewIndex(fsys)
}

// assetServer serves one static asset or reports found=false so the caller
// falls back to http.FileServer. *webassets.Index and *webassets.DiskIndex
// implement it.
type assetServer interface {
	Serve(w http.ResponseWriter, r *http.Request, p webassets.CachePolicy) bool
}

// staticAssetIndex holds the precompressed-asset server for the web
// server's current asset source (ws.assets or ws.assetsDisk).
//
// In production the source is fixed: NewWebServer builds the index once and
// every request takes the lock-free fast path. Tests, however, replace
// ws.assets or ws.assetsDisk after construction (see staticHandler), so each
// request compares the source it sees with the one the index was built from
// (a pointer or value comparison, never a hash) and rebuilds under the mutex
// only when they differ. A stale index is therefore never served, with one
// limit: the identity of the source is compared, not its contents, so a
// test that changes the same MapFS in place after the first request would
// still get the index of the earlier contents.
type staticAssetIndex struct {
	current atomic.Pointer[staticAssetSnapshot]
	mu      sync.Mutex // serializes rebuilds
}

type staticAssetSnapshot struct {
	fsys   fs.FS  // embedded source the server was built for, or nil
	dir    string // disk source the server was built for, or ""
	server assetServer
}

func (s *staticAssetSnapshot) matches(fsys fs.FS, dir string) bool {
	if dir != "" || s.dir != "" {
		return s.dir == dir
	}
	return sameFS(s.fsys, fsys)
}

// staticAssets returns the asset server for the current asset source, or
// nil when there is none (no assets, or the embedded index failed to build,
// in which case every request uses the http.FileServer fallback).
func (ws *WebServer) staticAssets() assetServer {
	fsys, dir := ws.assets, ws.assetsDisk
	if snap := ws.staticIndex.current.Load(); snap != nil && snap.matches(fsys, dir) {
		return snap.server
	}
	ws.staticIndex.mu.Lock()
	defer ws.staticIndex.mu.Unlock()
	if snap := ws.staticIndex.current.Load(); snap != nil && snap.matches(fsys, dir) {
		return snap.server
	}
	snap := &staticAssetSnapshot{fsys: fsys, dir: dir}
	switch {
	case dir != "":
		snap.server = webassets.NewDiskIndex(dir)
	case fsys != nil:
		ix, err := newIndexFor(fsys)
		if err != nil {
			ws.logger().Warn("Static asset index failed; serving assets without precompressed variants or ETags", "error", err)
		} else {
			ws.logger().Debug("Static asset index built", "files", ix.Len())
			snap.server = ix
		}
	}
	ws.staticIndex.current.Store(snap)
	return snap.server
}

// sameFS reports whether a and b are the same file system value. It never
// panics: maps, pointers and other reference kinds compare by identity,
// comparable values with ==, and anything else counts as different (the
// caller then rebuilds, which is correct, only slower).
func sameFS(a, b fs.FS) (same bool) {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Type() != vb.Type() {
		return false
	}
	switch va.Kind() {
	case reflect.Map, reflect.Pointer, reflect.UnsafePointer, reflect.Chan, reflect.Func, reflect.Slice:
		if va.Kind() == reflect.Slice && va.Len() != vb.Len() {
			return false
		}
		return va.Pointer() == vb.Pointer()
	}
	if !va.Type().Comparable() {
		return false
	}
	defer func() {
		if recover() != nil {
			same = false // a comparable type holding an incomparable interface value
		}
	}()
	return a == b
}
