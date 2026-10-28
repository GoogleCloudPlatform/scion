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

import "net/http"

// uploadFormMaxMemory returns the in-memory limit for multipart parsing on
// the template, harness-config and project workspace file upload handlers:
// maxUploadTotalSize, unless a test lowered it through
// uploadFormMemoryForTest. Only test code assigns that field; the source pin
// checks it.
func (s *Server) uploadFormMaxMemory() int64 {
	if s.uploadFormMemoryForTest > 0 {
		return s.uploadFormMemoryForTest
	}
	return maxUploadTotalSize
}

// parseMultipartUpload parses r's multipart form with the given in-memory
// limit and returns a cleanup func that removes any temporary files the parse
// wrote. Handlers receive a derived copy of the request, so the server's own
// form cleanup does not reach the copy's form; every handler that parses a
// multipart form must remove it itself.
//
// cleanup is non-nil even when err is non-nil: ParseMultipartForm can write
// temporary files and then return an error from the query string. Call it in
// the handler's own frame, deferred before the error check:
//
//	cleanup, err := parseMultipartUpload(r, maxMemory)
//	defer cleanup()
//	if err != nil {
//		...
//	}
//
// The helper must not defer the removal itself: that would delete the files
// before the handler reads them. TestMultipartFormSourcePin enforces both the
// helper's use and the defer placement.
func parseMultipartUpload(r *http.Request, maxMemory int64) (cleanup func(), err error) {
	err = r.ParseMultipartForm(maxMemory)
	return func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}, err
}
