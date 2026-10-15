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
)

// urlUserInfoRe matches the user info of a URL that url.Parse rejects: up
// to the last '@' before the path, query or fragment.
var urlUserInfoRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*://)?[^/?#]*@`)

// redactURLUserInfo returns raw with its user info redacted as
// config.RedactURL does. It is a copy of those rules (this package imports
// no scion package); change both together.
func redactURLUserInfo(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		if u.User == nil {
			return raw
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			return u.Redacted()
		}
		u.User = url.User("xxxxx")
		return u.String()
	}
	return urlUserInfoRe.ReplaceAllString(raw, "${1}xxxxx@")
}

// redactURLError returns err with the URL of the *url.Error in its chain
// shown without user info. It changes that *url.Error in place, so it is
// applied to the error net/http or url.Parse returns, before any wrapping
// (fmt.Errorf fixes the wrapped message when it is called).
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = redactURLUserInfo(ue.URL)
	}
	return err
}
