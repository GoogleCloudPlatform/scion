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

package hub

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util/ssrfclient"
)

// importSourceRefusedError is returned for a template or harness-config
// import source that a hosted hub does not fetch. Its message never contains
// the source URL, which may carry credentials.
type importSourceRefusedError struct{ msg string }

func (e *importSourceRefusedError) Error() string { return e.msg }

// checkImportSource refuses, on a hosted hub, import sources the hub must not
// fetch on a caller's behalf, following the skills-discover precedent
// (handlers_skills_discover.go):
//
//   - rclone connection strings (":local:/...", ":googlecloudstorage:...", any
//     ":backend:"), which would read the hub's own filesystem or use its
//     ambient cloud credentials;
//   - anything other than https, in particular plain http;
//   - an https URL whose host is a literal private, loopback or link-local
//     address. Host names are checked when dialed: hosted archive downloads use
//     importArchiveClient.
//
// A workstation hub accepts every remote URI, as before.
func (s *Server) checkImportSource(sourceURL string) error {
	if s.workstation {
		return nil
	}
	return checkHostedImportSource(sourceURL)
}

func checkHostedImportSource(sourceURL string) error {
	if strings.HasPrefix(sourceURL, ":") {
		return &importSourceRefusedError{"sourceUrl: rclone sources are not allowed on a hosted server; use an https:// URL"}
	}
	u, err := url.Parse(sourceURL)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" {
		return &importSourceRefusedError{"sourceUrl must be an https:// URL on a hosted server"}
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ssrfclient.IsBlockedPrivateOrLocal(ip) {
		return &importSourceRefusedError{"sourceUrl must not name a private, loopback or link-local address on a hosted server"}
	}
	return nil
}

// importArchiveClient returns the HTTP client for archive downloads during
// import: nil (http.DefaultClient) on a workstation hub, and on a hosted hub an
// SSRF-safe client that refuses private, loopback and link-local destinations
// and redirects.
func (s *Server) importArchiveClient() *http.Client {
	if s.workstation {
		return nil
	}
	return ssrfclient.NewClient(ssrfclient.Config{})
}

// importFetchDisallowedMessage answers a hosted archive download that the
// SSRF-safe client stopped. It is fixed, so the resolved address and the
// redirect target are not echoed.
const importFetchDisallowedMessage = "sourceUrl redirected or resolved to a disallowed address; on a hosted server give the final public https URL"

// mapImportFetchError replaces an error from the SSRF-safe archive client (a
// blocked address or a refused redirect) with importFetchDisallowedMessage.
// Other errors are returned unchanged.
func mapImportFetchError(err error) error {
	if errors.Is(err, ssrfclient.ErrBlockedAddress) || errors.Is(err, ssrfclient.ErrRedirectBlocked) {
		return &importSourceRefusedError{importFetchDisallowedMessage}
	}
	return err
}

// refuseImportSource writes a 400 invalid_request answer and returns true when
// checkImportSource refuses sourceURL. Handlers call it before fetching, so the
// refusal also reaches streaming (NDJSON) callers as a plain error answer.
func (s *Server) refuseImportSource(w http.ResponseWriter, sourceURL string) bool {
	err := s.checkImportSource(sourceURL)
	if err == nil {
		return false
	}
	var refused *importSourceRefusedError
	msg := "sourceUrl is not allowed"
	if errors.As(err, &refused) {
		msg = refused.msg
	}
	writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, msg, nil)
	return true
}
