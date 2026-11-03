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
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// IsSingleNameComponent reports whether v is exactly one local path
// component that can name a resource inside its directory (a template or a
// harness-config): not empty, valid UTF-8 without NUL, local
// (filepath.IsLocal), no "/" or "\" on any platform, not "." or "..", and not
// a remote source (no leading ":", as in an rclone connection string, and no
// "://").
func IsSingleNameComponent(v string) bool {
	if v == "" || !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
		return false
	}
	if v == "." || v == ".." || strings.ContainsAny(v, `/\`) {
		return false
	}
	if strings.HasPrefix(v, ":") || strings.Contains(v, "://") {
		return false
	}
	return filepath.IsLocal(v)
}
