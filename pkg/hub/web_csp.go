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
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
)

// webContentSecurityPolicy returns the CSP sent with every web response.
// Inline scripts run only with the per-response nonce, which the one
// inline script of the SPA shell (theme detection) carries; no other
// inline script runs. Script files are allowed from 'self': the bundled
// files, and any other script file served from the hub's own address.
//
// The client bundles its scripts, styles and Shoelace assets, so the only
// named external hosts are Google Fonts (stylesheet and font files) and
// Cloud Storage (signed upload and download URLs); img-src also allows any
// HTTPS image through its https: source.
//
// img-src allows blob: because the chat file preview fetches image bytes
// with credentials and renders them through URL.createObjectURL (gs://
// links, attachments, workspace files). A blob: URL matches neither 'self'
// nor https:, so without it the browser blocks the load and the preview
// shows a broken image. blob: is allowed for images only, never for
// scripts or frames.
func webContentSecurityPolicy(nonce string) string {
	return "default-src 'self'; " +
		"script-src 'self' 'nonce-" + nonce + "'; " +
		"style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
		"font-src 'self' https://fonts.gstatic.com; " +
		"img-src 'self' data: blob: https:; " +
		"connect-src 'self' data: ws: wss: http://localhost:* http://127.0.0.1:* https://storage.googleapis.com"
}

// cspReportPath is where browsers send policy reports for the web pages
// (Server.handleCSPReport).
const cspReportPath = "/api/v1/csp-reports"

// webTrustedTypesReportOnlyPolicy asks the browser to report, without
// blocking anything, each place the page hands a plain string to an API
// that parses it as markup or script. The reports go to the "csp" endpoint
// named in webReportingEndpoints.
const webTrustedTypesReportOnlyPolicy = "require-trusted-types-for 'script'; report-to csp"

// webReportingEndpoints names the "csp" report endpoint.
const webReportingEndpoints = `csp="` + cspReportPath + `"`

// setShellReportHeaders adds the report-only policy and its report
// endpoint to an SPA shell response.
func setShellReportHeaders(h http.Header) {
	h.Set("Content-Security-Policy-Report-Only", webTrustedTypesReportOnlyPolicy)
	h.Set("Reporting-Endpoints", webReportingEndpoints)
}

// scriptNonceBytes is the size of a script nonce before encoding.
const scriptNonceBytes = 16

// newScriptNonce returns a fresh script nonce: 16 random bytes, base64
// encoded with the URL alphabet and no padding, so the value goes into
// the HTML attribute unchanged.
func newScriptNonce() string {
	b := make([]byte, scriptNonceBytes)
	// crypto/rand.Read never returns an error; it aborts the program if
	// the system source fails.
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

type scriptNonceContextKey struct{}

// withScriptNonce records the nonce of the response's page policy.
func withScriptNonce(ctx context.Context, nonce string) context.Context {
	return context.WithValue(ctx, scriptNonceContextKey{}, nonce)
}

// scriptNonceFromContext returns the nonce securityHeadersMiddleware put
// in the request context, or "".
func scriptNonceFromContext(ctx context.Context) string {
	nonce, _ := ctx.Value(scriptNonceContextKey{}).(string)
	return nonce
}

// scriptNonceForShell returns the nonce the SPA shell puts on its inline
// script. It is normally the one securityHeadersMiddleware minted for this
// response. When the shell is served without that middleware, it mints a
// nonce and sets the page policy itself, so the shell's script and the
// policy always agree. Call it before the response header is written.
func scriptNonceForShell(w http.ResponseWriter, r *http.Request) string {
	if nonce := scriptNonceFromContext(r.Context()); nonce != "" {
		return nonce
	}
	nonce := newScriptNonce()
	w.Header().Set("Content-Security-Policy", webContentSecurityPolicy(nonce))
	return nonce
}
