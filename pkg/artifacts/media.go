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

package artifacts

import (
	"errors"
	"mime"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"
)

// extMediaTypes maps file extensions to media types. It is consulted before
// the client's Content-Type, so a file's stored type depends on its name
// rather than on what the uploader claimed, and does not vary with the
// host's mime.types.
var extMediaTypes = map[string]string{
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".txt":      "text/plain",
	".text":     "text/plain",
	".log":      "text/plain",
	".csv":      "text/csv",
	".tsv":      "text/tab-separated-values",
	".json":     "application/json",
	".yaml":     "application/yaml",
	".yml":      "application/yaml",
	".toml":     "application/toml",
	".xml":      "application/xml",
	".html":     "text/html",
	".htm":      "text/html",
	".css":      "text/css",
	".js":       "text/javascript",
	".mjs":      "text/javascript",
	".ts":       "text/plain",
	".go":       "text/plain",
	".py":       "text/plain",
	".sh":       "text/plain",
	".sql":      "text/plain",
	".diff":     "text/plain",
	".patch":    "text/plain",
	".png":      "image/png",
	".jpg":      "image/jpeg",
	".jpeg":     "image/jpeg",
	".gif":      "image/gif",
	".webp":     "image/webp",
	".svg":      "image/svg+xml",
	".pdf":      "application/pdf",
}

// detectMediaType returns the media type to store for a file named name,
// uploaded with the Content-Type header declared, whose first bytes are
// head. The extension wins; then a specific declared type; then sniffing.
// The result is a bare, lowercase media type without parameters.
// mediaTypeMarkdown is the media type whose entries are scanned for remote
// images.
const mediaTypeMarkdown = "text/markdown"

// mediaTypeHTML is the media type of HTML entries, shown through a view.
const mediaTypeHTML = "text/html"

func detectMediaType(name, declared string, head []byte) string {
	if mt, ok := extMediaTypes[strings.ToLower(path.Ext(name))]; ok {
		return mt
	}
	if declared != "" {
		if mt, _, err := mime.ParseMediaType(declared); err == nil {
			mt = strings.ToLower(mt)
			if mt != "application/octet-stream" && !strings.HasPrefix(mt, "multipart/") {
				return mt
			}
		}
	}
	mt, _, _ := mime.ParseMediaType(http.DetectContentType(head))
	if mt == "" {
		return "application/octet-stream"
	}
	return mt
}

// inlineSafe lists the media types the service lets a browser display
// inline from the hub's origin. None can run script. Everything else (HTML,
// SVG, PDF, unknown types) is served as an attachment; HTML rendering is a
// later phase's sandboxed frame (design D8), not a top-level document.
var inlineSafe = map[string]bool{
	"text/plain":                true,
	"text/markdown":             true,
	"text/csv":                  true,
	"text/tab-separated-values": true,
	"application/json":          true,
	"application/yaml":          true,
	"application/toml":          true,
	"image/png":                 true,
	"image/jpeg":                true,
	"image/gif":                 true,
	"image/webp":                true,
}

// isText reports whether a media type holds text, so it is served with a
// UTF-8 charset.
func isText(mt string) bool {
	return strings.HasPrefix(mt, "text/") || mt == "application/json" ||
		mt == "application/yaml" || mt == "application/toml" || mt == "application/xml"
}

// responseContentType is the Content-Type served for a stored media type.
func responseContentType(mt string) string {
	if isText(mt) {
		return mt + "; charset=utf-8"
	}
	return mt
}

// contentDisposition is the Content-Disposition served for a file: inline
// for inlineSafe types, attachment otherwise, with the file's base name.
func contentDisposition(mt, filePath string) string {
	disp := "attachment"
	if inlineSafe[mt] {
		disp = "inline"
	}
	if v := mime.FormatMediaType(disp, map[string]string{"filename": path.Base(filePath)}); v != "" {
		return v
	}
	return disp
}

const (
	maxPathBytes    = 1024
	maxSegmentBytes = 255
)

var errBadPath = errors.New("invalid file path")

// cleanFilePath validates a relative file path inside an artifact and
// returns it unchanged. It rejects anything that is not already a clean,
// relative, slash-separated path: empty or dot segments, a leading slash,
// backslashes, control characters, invalid UTF-8 and overlong names. Paths
// are never joined onto a filesystem location, but rejecting these forms
// keeps manifest paths canonical so one file has exactly one name.
func cleanFilePath(p string) (string, error) {
	if p == "" || len(p) > maxPathBytes || !utf8.ValidString(p) {
		return "", errBadPath
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", errBadPath
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || len(seg) > maxSegmentBytes {
			return "", errBadPath
		}
	}
	return p, nil
}
