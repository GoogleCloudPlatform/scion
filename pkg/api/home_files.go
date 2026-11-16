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

package api

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"strings"
)

const (
	// MaxHomeFiles is the maximum number of files a create request may write
	// into a newly provisioned agent's home directory.
	MaxHomeFiles = 4

	// MaxHomeFilesTotalBytes is the maximum combined byte length across all
	// HomeFiles in a single create request (64 KiB).
	MaxHomeFilesTotalBytes = 64 << 10

	homeFilesRequiredPrefix = ".scion/"
)

// HomeFile describes a small control-plane file the Hub asks the Runtime
// Broker to write into a newly provisioned agent's home directory at create
// time. Content is opaque to the broker and base64-encoded on the JSON wire.
type HomeFile struct {
	Path    string `json:"path"`
	Mode    uint32 `json:"mode"`
	Content []byte `json:"content"`
}

// CloneHomeFiles returns a deep copy of files so callers cannot mutate shared
// Content slices after validation. Returns nil when files is empty.
func CloneHomeFiles(files []HomeFile) []HomeFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]HomeFile, len(files))
	for i, f := range files {
		out[i] = HomeFile{
			Path:    f.Path,
			Mode:    f.Mode,
			Content: bytes.Clone(f.Content),
		}
	}
	return out
}

// ValidateHomeFiles enforces the bounds and path/mode invariants shared by the
// Hub (before dispatch) and the Runtime Broker (before provisioning). Error
// messages never include file Content.
func ValidateHomeFiles(files []HomeFile) error {
	if len(files) == 0 {
		return nil
	}
	if len(files) > MaxHomeFiles {
		return fmt.Errorf("too many home files: got %d, max %d", len(files), MaxHomeFiles)
	}

	seen := make(map[string]struct{}, len(files))
	totalBytes := 0
	for i, f := range files {
		if err := validateHomeFilePath(f.Path); err != nil {
			return fmt.Errorf("homeFiles[%d]: %w", i, err)
		}
		if f.Mode != 0o600 && f.Mode != 0o644 {
			return fmt.Errorf("homeFiles[%d] (%q): invalid mode %#o (allowed: 0600 or 0644)", i, f.Path, f.Mode)
		}
		if _, dup := seen[f.Path]; dup {
			return fmt.Errorf("homeFiles[%d]: duplicate path %q", i, f.Path)
		}
		seen[f.Path] = struct{}{}

		if len(f.Content) > MaxHomeFilesTotalBytes-totalBytes {
			return fmt.Errorf("homeFiles total content exceeds %d bytes", MaxHomeFilesTotalBytes)
		}
		totalBytes += len(f.Content)
	}
	return nil
}

func validateHomeFilePath(p string) error {
	if p == "" {
		return fmt.Errorf("path must not be empty")
	}
	if strings.IndexByte(p, 0) >= 0 {
		return fmt.Errorf("path must not contain NUL byte")
	}
	if strings.ContainsRune(p, '\\') {
		return fmt.Errorf("path must not contain backslash")
	}
	if strings.HasPrefix(p, "/") || hasWindowsDrivePrefix(p) {
		return fmt.Errorf("path must be relative")
	}
	if slices.Contains(strings.Split(p, "/"), "..") {
		return fmt.Errorf("path must not contain '..' segment")
	}
	if path.Clean(p) != p {
		return fmt.Errorf("path %q must be in clean slash-separated form", p)
	}
	if !strings.HasPrefix(p, homeFilesRequiredPrefix) {
		return fmt.Errorf("path %q must start with %q", p, homeFilesRequiredPrefix)
	}
	return nil
}

func hasWindowsDrivePrefix(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	c := p[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
