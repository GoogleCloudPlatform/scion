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

package util

import (
	"net/url"
	"strings"
)

// labelRedacted replaces any part of a printed config key name that is not
// positively known to be safe.
const labelRedacted = "<redacted>"

// safeSubsectionLabel returns a printable form of a config subsection that
// carries no userinfo, query, fragment, path or other value-bearing part. It
// is used only for names that are printed or recorded (refusal keys and the
// stripped-key list), never for config content kept for git. Anything it
// cannot positively classify becomes "<redacted>". In order:
//   - a credential subsection is always "<redacted>";
//   - a hasconfig:remote.*.url: prefix is printed in lower case, followed by
//     safeURLLabel of the rest. It is matched case-insensitively on purpose,
//     which can only over-redact; git itself matches it case-sensitively;
//   - a subsection made only of letters, digits, '.', '_', '/' and '-' is
//     returned unchanged;
//   - anything else is safeURLLabel(sub).
func safeSubsectionLabel(section, sub string) string {
	if section == "credential" {
		return labelRedacted
	}
	if hasPrefixFold(sub, hasConfigURLPrefix) {
		return hasConfigURLPrefix + safeURLLabel(sub[len(hasConfigURLPrefix):])
	}
	if allBytesIn(sub, isPlainLabelByte) {
		return sub
	}
	return safeURLLabel(sub)
}

// safeURLLabel returns scheme://host for a URL, followed by "/<redacted>" when
// the URL has userinfo, a path other than "/", a query or a fragment (or "/"
// when the path is exactly "/"). It returns "<redacted>" unless the string
// parses with a scheme, no opaque part, a host made only of letters, digits,
// '.', '-', ':', '[' and ']', and every '@' accounted for by the userinfo.
func safeURLLabel(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Opaque != "" || u.Host == "" || !allBytesIn(u.Host, isHostLabelByte) {
		return labelRedacted
	}
	wantAt := 0
	if u.User != nil {
		wantAt = 1
	}
	if strings.Count(s, "@") != wantAt {
		return labelRedacted
	}
	out := strings.ToLower(u.Scheme) + "://" + u.Host
	switch {
	case u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery:
		return out + "/" + labelRedacted
	case u.Path == "/":
		return out + "/"
	default:
		return out
	}
}

// hasPrefixFold reports whether s starts with prefix, ignoring ASCII case.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func allBytesIn(s string, ok func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if !ok(s[i]) {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// isPlainLabelByte: letters, digits, '.', '_', '/' and '-'.
func isPlainLabelByte(c byte) bool {
	return isAlnum(c) || c == '.' || c == '_' || c == '/' || c == '-'
}

// isHostLabelByte: letters, digits, '.', '-', ':', '[' and ']'.
func isHostLabelByte(c byte) bool {
	return isAlnum(c) || c == '.' || c == '-' || c == ':' || c == '[' || c == ']'
}
