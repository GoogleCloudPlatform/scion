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

package config

import (
	"net/url"
	"strings"
)

// The query and fragment rules of RedactURL. pkg/apiclient keeps a copy of
// them (redact.go), since it imports no scion package; change both
// together.

// urlCredentialParamNames are query and fragment parameter names whose
// value RedactURL masks, compared lower-cased. A name that contains
// "token", "secret", "password", "passwd", "apikey", "api_key" or
// "api-key", or that ends in "_key" or "-key", is masked too (see
// isURLCredentialParam).
var urlCredentialParamNames = map[string]bool{
	"key":               true,
	"auth":              true,
	"authorization":     true,
	"code":              true,
	"credential":        true,
	"assertion":         true,
	"client_assertion":  true,
	"sig":               true,
	"signature":         true,
	"x-amz-credential":  true,
	"x-amz-signature":   true,
	"x-goog-credential": true,
	"x-goog-signature":  true,
}

// isURLCredentialParam reports whether a query or fragment parameter name
// (already unescaped) names a credential.
func isURLCredentialParam(name string) bool {
	n := strings.ToLower(name)
	if urlCredentialParamNames[n] {
		return true
	}
	for _, s := range []string{"token", "secret", "password", "passwd", "apikey", "api_key", "api-key"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return strings.HasSuffix(n, "_key") || strings.HasSuffix(n, "-key")
}

// redactURLParams returns raw (a URL, or the part of one from its path
// on) with the value of every credential parameter in its query and its
// fragment replaced by xxxxx. Every other byte, including the order and
// spelling of the other parameters, is kept.
func redactURLParams(raw string) string {
	rest, fragment, hasFragment := strings.Cut(raw, "#")
	base, query, hasQuery := strings.Cut(rest, "?")
	out := base
	if hasQuery {
		out += "?" + redactParamList(query)
	}
	if hasFragment {
		out += "#" + redactParamList(fragment)
	}
	return out
}

// redactParamList masks the credential values of an &-separated list of
// name=value pairs. A name that does not unescape is compared as written.
func redactParamList(list string) string {
	if !strings.Contains(list, "=") {
		return list
	}
	pairs := strings.Split(list, "&")
	for i, pair := range pairs {
		name, _, hasValue := strings.Cut(pair, "=")
		if !hasValue {
			continue
		}
		unescaped, err := url.QueryUnescape(name)
		if err != nil {
			unescaped = name
		}
		if isURLCredentialParam(unescaped) {
			pairs[i] = name + "=" + redactedSecret
		}
	}
	return strings.Join(pairs, "&")
}
