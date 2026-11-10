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

package messaging

import (
	"bytes"
	"path/filepath"
	"strings"
)

// IsHTMLOrSVG reports whether an attachment named name, whose first bytes
// are head, is HTML or SVG, by extension or by content. Such files are not
// accepted as message attachments for agents; the CLI and the hub's broker
// attachment upload both refuse them with this one check.
func IsHTMLOrSVG(name string, head []byte) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".html", ".htm", ".svg":
		return true
	}
	lower := bytes.ToLower(head)
	return bytes.Contains(lower, []byte("<html")) || bytes.Contains(lower, []byte("<svg"))
}
