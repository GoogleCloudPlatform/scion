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

import "strings"

// skillURITokenParam is the only query parameter a skill URI supports:
// ?token=SECRET_NAME names the secret used to resolve the skill. The name is
// matched case-sensitively, as in NormalizeSkillURI and the resolver in
// pkg/agent/github_uri.go.
const skillURITokenParam = "token"

// SkillURITokenParam returns the value of the supported token parameter in
// rawQuery (a URL's raw query, without the leading '?'), and whether it is
// present. Every other parameter is ignored, so a caller that rebuilds the
// query from this value passes through only the supported parameter.
//
// The rules:
//
//   - Parameters are split on '&'. A parameter counts only when its raw name
//     is exactly "token" and it has an '='. Other names, including other
//     casings ("Token", "TOKEN") and percent-encoded spellings, are ignored.
//   - When "token" is repeated, the first occurrence wins, as in
//     url.Values.Get.
//   - The value is returned byte for byte: not decoded, re-encoded or
//     validated. Check it with IsValidSkillTokenSecretName.
func SkillURITokenParam(rawQuery string) (value string, ok bool) {
	for _, param := range strings.Split(rawQuery, "&") {
		name, v, hasValue := strings.Cut(param, "=")
		if hasValue && name == skillURITokenParam {
			return v, true
		}
	}
	return "", false
}

// IsValidSkillTokenSecretName reports whether name is an acceptable
// ?token= value: a secret name of the form [A-Z][A-Z0-9_]*.
//
// This is input hygiene, not a redaction boundary. A secret name and a
// pasted secret cannot be told apart from the string, so callers must not
// rely on this check to keep values out of logs or responses.
func IsValidSkillTokenSecretName(name string) bool {
	return validGHSecretName.MatchString(name)
}

// SkillURIWithoutQuery returns uri with any query (from the first '?') cut
// off. Use it wherever a candidate skill URI is logged or quoted in an error,
// so that whatever the caller put in the query is never written out.
func SkillURIWithoutQuery(uri string) string {
	if i := strings.IndexByte(uri, '?'); i >= 0 {
		return uri[:i]
	}
	return uri
}
