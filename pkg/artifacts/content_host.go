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

package artifacts

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// The hub's content host (server.hub.content_url, ptone/scion#7107 phase A)
// is a second public origin that routes back to this hub, on which shared
// artifacts are served. A request is a content-host request exactly when
// its Host, as the hub receives it (r.Host), names that origin's host and
// port. One detection, ContentHostRequest, decides it; ContentHostGuard
// applies it once per request and records the result in the request
// context, and the confinement, the credential drop, the response headers
// and the view CSP source all follow that record. X-Forwarded-Host,
// Forwarded and every other header a requester can set are never read: a
// proxy in front of the content host must preserve Host, or the hub cannot
// tell the origins apart.

// ContentHost is a parsed content host (ParseContentHost). The zero value is
// no content host: it matches no request.
type ContentHost struct {
	host string // lower-cased host name, no trailing dot
	port string // explicit or the scheme's default port
	def  string // the scheme's default port
	// cspHost is the host-source of the view CSP: host, plus the port
	// when it is not the scheme's default.
	cspHost string
}

// ParseContentHost parses a content URL with config.ParseContentURL, the
// one parse validation and the hub's conflict check use too, so the guard
// never accepts a value the settings would refuse. An empty or invalid value
// gives no content host, no confinement and no shareUrl. The hub parses the
// value once per settings apply and hands the result to ContentHostGuard, so
// no request parses it.
func ParseContentHost(raw string) (ContentHost, bool) {
	p, ok := config.ParseContentURL(raw)
	if !ok {
		return ContentHost{}, false
	}
	o := ContentHost{host: p.Host, port: p.Port, def: p.DefaultPort, cspHost: p.Host}
	if p.Port != p.DefaultPort {
		o.cspHost += ":" + p.Port
	}
	// Kept as defence in depth: the CSP source must be a plain host-source
	// whatever the parser accepts.
	if !validCSPHost(o.cspHost) {
		return ContentHost{}, false
	}
	return o, true
}

// IsSet reports whether o is a content host (not the zero value).
func (o ContentHost) IsSet() bool { return o.host != "" }

// matchesHost reports whether a request Host names this origin: the host
// compared case-insensitively (a single trailing dot ignored, since it
// names the same host), the port exactly, with the scheme's default port
// implied when the Host carries none.
func (o ContentHost) matchesHost(reqHost string) bool {
	host, port := reqHost, ""
	if h, p, err := net.SplitHostPort(reqHost); err == nil {
		host, port = h, p
	}
	if port == "" {
		port = o.def
	}
	return strings.TrimSuffix(strings.ToLower(host), ".") == o.host && port == o.port
}

// ContentHostRequest reports whether r is a request on the content host
// contentURL: whether r.Host, the Host the hub received, names that
// origin's host and port. It reads nothing else from the request. An empty
// or unparseable contentURL matches no request.
func ContentHostRequest(r *http.Request, contentURL string) bool {
	o, ok := ParseContentHost(contentURL)
	return ok && o.matchesHost(r.Host)
}

// contentHostKey marks a request context as a content-host request; its
// value is the origin's CSP host-source.
type contentHostKey struct{}

// contentHostCSPHost returns the CSP host-source of the content host when
// ContentHostGuard marked ctx as a content-host request.
func contentHostCSPHost(ctx context.Context) (string, bool) {
	h, ok := ctx.Value(contentHostKey{}).(string)
	return h, ok && h != ""
}

// ContentHostGuard returns the outermost middleware for the content host
// (security review conditions 2 and 3, and C1-1). contentHost yields the
// parsed content host in effect for r (the zero ContentHost when none is
// set), read on every request so a settings change applies without a
// restart; the caller parses the value once per change (ParseContentHost),
// not per request, and may hand the guard the value it already loaded for
// r so the whole request sees one snapshot.
//
// On a request that is not a content-host request (ContentHostRequest), the
// middleware does nothing. On one that is:
//
//   - every response carries Referrer-Policy: no-referrer,
//     Cache-Control: no-store and Access-Control-Allow-Origin: * with no
//     Access-Control-Allow-Credentials, whatever the inner handler sets,
//     the 404 below and the share link's 303 included;
//   - only a request servesOnHost admits is served: the hub's table of the
//     route families the content host serves (artifact share links and
//     artifact views). Every other request, the web app and every other API
//     route included, answers 404 before any inner handler or
//     authentication runs, and refused (when not nil) is told of it. A nil
//     servesOnHost admits nothing;
//   - the Cookie and Authorization headers are removed, and then
//     stripCredentials (when not nil) removes the host's other credential
//     headers, so no ambient credential can authenticate on that origin;
//   - the request context is marked, so the view route builds its CSP on
//     the content host.
//
// The inner handler gets a clone; the caller's request is not changed.
func ContentHostGuard(contentHost func(*http.Request) ContentHost, servesOnHost func(*http.Request) bool, stripCredentials func(http.Header), refused func(*http.Request)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			o := contentHost(r)
			if !o.IsSet() || !o.matchesHost(r.Host) {
				next.ServeHTTP(w, r)
				return
			}
			cw := &contentHostWriter{ResponseWriter: w}
			if servesOnHost == nil || !servesOnHost(r) {
				if refused != nil {
					refused(r)
				}
				writeNotFound(cw)
				return
			}
			r2 := r.Clone(context.WithValue(r.Context(), contentHostKey{}, o.cspHost))
			r2.Header.Del("Cookie")
			r2.Header.Del("Authorization")
			if stripCredentials != nil {
				stripCredentials(r2.Header)
			}
			next.ServeHTTP(cw, r2)
		})
	}
}

// contentHostWriter sets the content host's fixed response headers just
// before the status line goes out, so no inner handler or middleware can
// drop or replace them: Referrer-Policy: no-referrer keeps a capability in
// the address bar out of any Referer, and Cache-Control: no-store keeps a
// capability-bearing response out of every cache (C1-1);
// Access-Control-Allow-Origin: * with no Access-Control-Allow-Credentials
// never echoes a requester's Origin and never admits credentials (the
// CORS rule port view follows too).
type contentHostWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *contentHostWriter) setHeaders() {
	if w.wrote {
		return
	}
	w.wrote = true
	h := w.Header()
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Del("Access-Control-Allow-Credentials")
}

func (w *contentHostWriter) WriteHeader(code int) {
	w.setHeaders()
	w.ResponseWriter.WriteHeader(code)
}

func (w *contentHostWriter) Write(b []byte) (int, error) {
	w.setHeaders()
	return w.ResponseWriter.Write(b)
}

// Flush passes a flush through when the wrapped writer supports it.
func (w *contentHostWriter) Flush() {
	w.setHeaders()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap gives http.ResponseController the wrapped writer.
func (w *contentHostWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// shareURL returns the absolute share link on the content host for the
// hub-relative link path rel, or "" when limits carry no valid content URL.
// It is built from the configured value only.
func shareURL(l Limits, rel string) string {
	if _, ok := ParseContentHost(l.ContentURL); !ok {
		return ""
	}
	return strings.TrimSuffix(l.ContentURL, "/") + rel
}

// hubCSPHost returns the CSP host-source of the hub's configured public
// origin, or "" when none is configured or it cannot be named.
func hubCSPHost(publicOrigin string) string {
	u, err := url.Parse(publicOrigin)
	if err != nil || u.Host == "" {
		return ""
	}
	h := strings.ToLower(u.Host)
	if !validCSPHost(h) {
		return ""
	}
	return h
}
