package hub

import (
	"net/http"
	"strings"
)

// frontProxyIdentityHeaders are the identity headers that the supported front
// proxies set (including unsigned copies). They are not forwarded to a proxied
// agent app, whichever provider is configured. The configured provider's own
// header names are added (see stripFrontProxyIdentityHeaders), so a non-default
// jwt header name is handled too.
var frontProxyIdentityHeaders = []string{
	DefaultJWTHeader,                  // X-Auth-Proxy-JWT (generic jwt provider default)
	IAPAssertionHeader,                // X-Goog-IAP-JWT-Assertion (Google IAP)
	"X-Goog-Authenticated-User-Email", // IAP unsigned convenience header
	"X-Goog-Authenticated-User-Id",    // IAP unsigned convenience header
}

// stripFrontProxyIdentityHeaders removes the front proxy's identity headers
// from h, so a request proxied to an agent's port reaches the app without them.
// Both port-proxy transports (and the page-view surface) call it. It removes
// the built-in set above plus the configured provider's header names
// (AssertionHeaders), matched case-insensitively.
func (s *Server) stripFrontProxyIdentityHeaders(h http.Header) {
	for _, name := range frontProxyIdentityHeaders {
		h.Del(name)
	}
	if s.authConfig.ProxyAuthenticator != nil {
		for _, name := range s.authConfig.ProxyAuthenticator.AssertionHeaders() {
			if strings.TrimSpace(name) != "" {
				h.Del(name)
			}
		}
	}
}
