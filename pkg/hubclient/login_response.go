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

package hubclient

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// iapGuideURL is the documentation page for using Scion behind IAP.
const iapGuideURL = "https://googlecloudplatform.github.io/scion/hosted/ha/auth-proxy-iap/"

// googleSignInHost is where IAP redirects a request without a valid IAP
// credential.
const googleSignInHost = "accounts.google.com"

// loginResponsePeekBytes is how much of a login response body is inspected
// before it is decoded.
const loginResponsePeekBytes = 512

// iapInvalidCredentialsPrefix starts the plain-text body IAP sends with a
// 401 or 403 when a request carries no valid IAP credential.
const iapInvalidCredentialsPrefix = "Invalid IAP credentials"

// ErrHubBehindIAP reports that Identity-Aware Proxy (IAP), not the hub,
// answered a login request.
var ErrHubBehindIAP = errors.New("hub appears to be behind Identity-Aware Proxy (IAP)")

// ErrHubHTMLResponse reports that a login request got an HTML page instead
// of a JSON API response, from something other than IAP.
var ErrHubHTMLResponse = errors.New("hub returned an HTML page instead of a JSON API response")

// IAPResponseError is returned when IAP answered a login request. It names
// only the hub origin; it never carries the response body or the redirect
// target.
type IAPResponseError struct {
	// HubOrigin is the scheme and host of the hub URL.
	HubOrigin string
	// TransportCredentialSent reports whether the client was configured
	// with a transport credential (SCION_TRANSPORT_MODE and token).
	TransportCredentialSent bool
}

func (e *IAPResponseError) Error() string {
	if e.TransportCredentialSent {
		return fmt.Sprintf("the hub at %s appears to be behind Identity-Aware Proxy (IAP), and IAP did not accept the transport token: "+
			"check that it has not expired, was minted for the IAP OAuth client ID as its audience with the email claim included, "+
			"and that its identity has the IAP-secured Web App User role; see the IAP auth guide: %s",
			e.HubOrigin, iapGuideURL)
	}
	return fmt.Sprintf("the hub at %s appears to be behind Identity-Aware Proxy (IAP): IAP answered instead of the hub. "+
		"Use the IAP transport: set SCION_TRANSPORT_MODE=iap, SCION_TRANSPORT_TOKEN or SCION_TRANSPORT_TOKEN_FILE "+
		"to an OIDC token minted for the IAP OAuth client ID (the audience) with the email claim included, "+
		"and SCION_HUB_TOKEN to a user access token; see the IAP auth guide: %s",
		e.HubOrigin, iapGuideURL)
}

// Is makes errors.Is(err, ErrHubBehindIAP) match.
func (e *IAPResponseError) Is(target error) bool { return target == ErrHubBehindIAP }

// HTMLResponseError is returned when a login request got an HTML page that
// is not recognisably from IAP. The page itself is not included.
type HTMLResponseError struct {
	HubOrigin  string
	StatusCode int
}

func (e *HTMLResponseError) Error() string {
	return fmt.Sprintf("the hub at %s returned an HTML page (HTTP %d) instead of a JSON API response; "+
		"check the hub URL and any proxy in front of the hub", e.HubOrigin, e.StatusCode)
}

// Is makes errors.Is(err, ErrHubHTMLResponse) match.
func (e *HTMLResponseError) Is(target error) bool { return target == ErrHubHTMLResponse }

// checkLoginResponse inspects a response to one of the early login calls
// before it is decoded. It returns an *IAPResponseError when IAP answered
// (an IAP-generated response, IAP's plain-text "Invalid IAP credentials"
// 401/403, or a redirect that ended on the Google sign-in page) and an
// *HTMLResponseError for any other HTML page. On a nil
// return resp.Body still yields the full, unread body. On an error the
// body is closed.
func (c *client) checkLoginResponse(resp *http.Response) error {
	var peeked []byte
	if resp.Body != nil && resp.Body != http.NoBody {
		br := bufio.NewReaderSize(resp.Body, loginResponsePeekBytes)
		// A short body returns io.EOF here; any other read error surfaces
		// again when the body is decoded.
		peeked, _ = br.Peek(loginResponsePeekBytes)
		resp.Body = struct {
			io.Reader
			io.Closer
		}{br, resp.Body}
	}

	origin := hubOrigin(c.transport.BaseURL)
	if isIAPResponse(resp, peeked) {
		closeBody(resp)
		return &IAPResponseError{HubOrigin: origin, TransportCredentialSent: c.hasTransportCredential()}
	}
	if isHTMLResponse(resp, peeked) {
		closeBody(resp)
		return &HTMLResponseError{HubOrigin: origin, StatusCode: resp.StatusCode}
	}
	return nil
}

// hasTransportCredential reports whether the client's HTTP transport adds a
// transport credential (transportauth.Wrap).
func (c *client) hasTransportCredential() bool {
	hc := c.transport.HTTPClient
	if hc == nil {
		return false
	}
	_, ok := hc.Transport.(apiclient.HubOnlyRoundTripper)
	return ok
}

// isIAPResponse reports whether IAP generated resp (by its marker header,
// or by its plain-text 401/403 body), or resp is the Google sign-in page
// that IAP redirected to.
func isIAPResponse(resp *http.Response, peeked []byte) bool {
	if strings.EqualFold(resp.Header.Get("X-Goog-IAP-Generated-Response"), "true") {
		return true
	}
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) &&
		bytes.HasPrefix(bytes.TrimLeft(peeked, " \t\r\n"), []byte(iapInvalidCredentialsPrefix)) {
		return true
	}
	if resp.Request != nil && resp.Request.URL != nil &&
		strings.EqualFold(resp.Request.URL.Hostname(), googleSignInHost) {
		return true
	}
	return false
}

// isHTMLResponse reports whether resp is an HTML page: its media type is
// HTML, or its body starts with '<' after leading whitespace.
func isHTMLResponse(resp *http.Response, peeked []byte) bool {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		if mt, _, err := mime.ParseMediaType(ct); err == nil {
			switch mt {
			case "text/html", "application/xhtml+xml":
				return true
			}
		}
	}
	trimmed := bytes.TrimLeft(peeked, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '<'
}

// hubOrigin returns the scheme and host of baseURL, without path, query or
// user information.
func hubOrigin(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return "the configured URL"
	}
	return u.Scheme + "://" + u.Host
}

func closeBody(resp *http.Response) {
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
}
