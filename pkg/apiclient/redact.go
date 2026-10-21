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

package apiclient

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// urlUserInfoRe matches the user info of a URL that url.Parse rejects: up
// to the last '@' before the path, query or fragment.
var urlUserInfoRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*://)?[^/?#]*@`)

// redactedSecret replaces each credential redactURLCredentials masks.
const redactedSecret = "xxxxx"

// redactURLCredentials returns raw with its credentials redacted as
// config.RedactURL does: the user info, and the value of every query or
// fragment parameter with a credential-like name. It is a copy of those
// rules (this package imports no scion package); change both together.
func redactURLCredentials(raw string) string {
	s := raw
	if u, err := url.Parse(raw); err == nil {
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				s = u.Redacted()
			} else {
				u.User = url.User(redactedSecret)
				s = u.String()
			}
		}
	} else {
		s = urlUserInfoRe.ReplaceAllString(raw, "${1}"+redactedSecret+"@")
	}
	return redactURLParams(s)
}

// urlCredentialParamNames is config.urlCredentialParamNames.
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

// isURLCredentialParam is config.isURLCredentialParam.
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

// redactURLParams is config.redactURLParams.
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

// redactParamList is config.redactParamList.
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

// redactURLError returns err with the URL of the *url.Error in its chain
// shown without its credentials (see redactURLCredentials). It changes that *url.Error in place, so it is
// applied to the error net/http or url.Parse returns, before any wrapping
// (fmt.Errorf fixes the wrapped message when it is called).
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = redactURLCredentials(ue.URL)
	}
	return err
}
