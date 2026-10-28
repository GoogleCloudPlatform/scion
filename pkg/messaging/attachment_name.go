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
	"strings"
	"unicode/utf8"
)

// MaxAttachmentNameRunes is the most runes a delivered attachment name has.
const MaxAttachmentNameRunes = 120

// ReduceAttachmentName reduces an attachment name to the name it is
// delivered under in an agent's inbox (ptone/scion#4596): [A-Za-z0-9._-]
// only (every other rune becomes '_'), at most MaxAttachmentNameRunes runes,
// and never starting with '.' or '-' (such a name gets an "attachment-"
// prefix). The hub reduces names with it, and the CLI uses it to refuse two
// files that would land under one name before it uploads them.
func ReduceAttachmentName(name string) string {
	var b strings.Builder
	n := 0
	for _, r := range name {
		if n == MaxAttachmentNameRunes {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		n++
	}
	out := b.String()
	if out == "" {
		return "attachment"
	}
	if out[0] == '.' || out[0] == '-' {
		out = "attachment-" + out
		if utf8.RuneCountInString(out) > MaxAttachmentNameRunes {
			out = out[:MaxAttachmentNameRunes]
		}
	}
	return out
}
