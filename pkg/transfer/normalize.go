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
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// NormalizeFileContent normalizes line endings from CRLF to LF for text files.
// Binary content (detected by the presence of null bytes or invalid UTF-8 in
// the first 8KB) is returned unchanged.
func NormalizeFileContent(data []byte) []byte {
	if !isTextContent(data) {
		return data
	}
	if !bytes.Contains(data, []byte("\r\n")) {
		return data
	}
	return bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
}

// isTextContent returns true if data appears to be text (UTF-8, no null bytes).
// It inspects at most the first 8KB.
func isTextContent(data []byte) bool {
	sample := data
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	if bytes.ContainsRune(sample, 0) {
		return false
	}
	return utf8.Valid(sample)
}

// binaryExtensions are file extensions that should never be normalized.
var binaryExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".ico": true, ".bmp": true, ".webp": true, ".svg": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".zip": true, ".tar": true, ".gz": true, ".bz2": true, ".xz": true,
	".pdf": true, ".exe": true, ".dll": true, ".so": true, ".dylib": true,
	".bin": true, ".dat": true, ".db": true, ".sqlite": true,
}

// NormalizeDir walks dir and normalizes line endings (CRLF → LF) for all text
// files in place. Binary files are left unchanged.
func NormalizeDir(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if binaryExtensions[ext] {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		normalized := NormalizeFileContent(data)
		if len(normalized) == len(data) && bytes.Equal(normalized, data) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(path, normalized, info.Mode())
	})
}
