package hub

import (
	"net/http"
	"net/url"
	"strings"
)

// proxyModeWriteGuard applies two request checks to state-changing /api/v1
// requests when the hub runs behind an authenticating front proxy
// (AuthMode == "proxy"). It runs before authentication and is a no-op in every
// other auth mode.
//
// First check (every POST, PUT, PATCH and DELETE under /api/v1): the request's
// Sec-Fetch-Site and Origin headers are compared with the request's Host (see
// requestIsCrossSite). A request that does not match gets 403.
//
// Second check (a POST from a browser with a non-empty body): the body must use
// a content type the route accepts: application/json; multipart/form-data on an
// upload route (see isMultipartUploadRoute); or a browser policy report type on
// the policy report route (see isPolicyReportRoute). Other requests, including
// ones with an absent or unparseable content type, get 415. PUT, PATCH and
// DELETE are left to the first check, since they legitimately carry file
// bodies.
//
// The /api/v1/portview/ route is not checked: it authorizes by capability and
// serves an agent app's own page requests.
func (s *Server) proxyModeWriteGuard(next http.Handler) http.Handler {
	if s.authConfig.AuthMode != proxyAuthMode {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isStateChangingAPIRequest(r) || isPortViewPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// First check: where the request comes from.
		if requestIsCrossSite(r) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"this request is not accepted on this endpoint", nil)
			return
		}
		// Second check: the body content type (browser POSTs only).
		if r.Method == http.MethodPost && requestHasOriginSignal(r) && !acceptableWriteBody(r) {
			writeError(w, http.StatusUnsupportedMediaType, errCodeUnsupportedMediaType,
				"this endpoint requires an application/json request body", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// proxyAuthMode is the AuthMode value for a hub behind an authenticating proxy.
const proxyAuthMode = "proxy"

// errCodeUnsupportedMediaType is the API error code for a refused body content
// type (HTTP 415).
const errCodeUnsupportedMediaType = "unsupported_media_type"

// portViewPathPrefix is the capability-only page-view route, which the guard
// does not check (see the doc comment).
const portViewPathPrefix = "/api/v1/portview/"

func isPortViewPath(path string) bool { return strings.HasPrefix(path, portViewPathPrefix) }

// multipartUploadRoutes are the state-changing /api/v1 routes that receive a
// multipart/form-data body (file and list uploads from the hub's web UI or from
// agents). In proxy auth mode only these accept multipart from a browser; other
// routes take application/json. Each entry is a slash-separated segment pattern
// where "*" matches exactly one path segment (a resource ID or name); a request
// path matches only when its segment count and every literal segment are equal,
// so a pattern never matches a sibling or deeper route. Keep it in sync with the
// router.
var multipartUploadRoutes = []string{
	"/api/v1/admin/allow-list/import",        // allow-list CSV import
	"/api/v1/admin/users/invite/bulk",        // bulk/batch invite CSV import
	"/api/v1/chat/attachments",               // message attachment upload (user and agent)
	"/api/v1/templates/*/files",              // template file upload
	"/api/v1/harness-configs/*/files",        // harness-config file upload
	"/api/v1/skills/*/versions",              // skill version publish (multipart)
	"/api/v1/projects/*/workspace/files",     // project workspace file upload
	"/api/v1/projects/*/shared-dirs/*/files", // shared-dir file upload
}

// isMultipartUploadRoute reports whether path matches one of the upload-route
// patterns (see multipartUploadRoutes): equal segment count, literal segments
// equal, "*" matching any single non-empty segment.
func isMultipartUploadRoute(path string) bool {
	segs := strings.Split(strings.TrimRight(path, "/"), "/")
	for _, pattern := range multipartUploadRoutes {
		if segmentsMatch(strings.Split(pattern, "/"), segs) {
			return true
		}
	}
	return false
}

// segmentsMatch reports whether path segments match the pattern segments, with
// "*" in the pattern matching exactly one non-empty segment.
func segmentsMatch(pattern, segs []string) bool {
	if len(pattern) != len(segs) {
		return false
	}
	for i, p := range pattern {
		if p == "*" {
			if segs[i] == "" {
				return false
			}
			continue
		}
		if p != segs[i] {
			return false
		}
	}
	return true
}

// isStateChangingAPIRequest reports whether r targets an /api/v1 route with a
// state-changing method.
func isStateChangingAPIRequest(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		return false
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// requestHasOriginSignal reports whether r carries the Sec-Fetch-Site or Origin
// header, which browsers send. A client that is not a browser (an agent,
// broker, CLI or server-to-server webhook) usually sends neither.
func requestHasOriginSignal(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Site") != "" || r.Header.Get("Origin") != ""
}

// acceptableWriteBody reports whether a POST request's body content type is
// allowed: application/json, multipart/form-data on an upload route, or a
// browser policy report type on the policy report route. A request with no
// body is allowed. An absent, empty or unparseable content type on a request
// that has a body is refused.
func acceptableWriteBody(r *http.Request) bool {
	if r.ContentLength == 0 {
		return true // no body to classify
	}
	mt, ok := requestMediaType(r)
	if !ok {
		return false // absent/empty content type with a body, or unparseable
	}
	switch mt {
	case "application/json":
		return true
	case "multipart/form-data":
		return isMultipartUploadRoute(r.URL.Path)
	case policyReportMediaType, legacyPolicyReportMediaType:
		return isPolicyReportRoute(r.URL.Path)
	default:
		return false
	}
}

// Browser policy reports (Server.handleCSPReport) arrive in one of these two
// media types. Browsers choose the type, so the report route accepts both.
const (
	policyReportMediaType       = "application/reports+json"
	legacyPolicyReportMediaType = "application/csp-report"
)

// isPolicyReportRoute reports whether path is exactly the browser policy report
// route (cspReportPath). The report media types are accepted there and nowhere
// else; the route needs no sign-in and only writes log records.
func isPolicyReportRoute(path string) bool { return path == cspReportPath }

// requestMediaType returns the lower-cased media-type essence of r's
// Content-Type (the token before any parameters), and whether it is a
// well-formed "type/subtype". Only the essence is read, the way browsers read
// it, so the result does not depend on the parameters (a missing value, a
// duplicate parameter or an unterminated quote). An absent or empty
// Content-Type returns ("", false).
func requestMediaType(r *http.Request) (string, bool) {
	ct := r.Header.Get("Content-Type")
	essence := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	if essence == "" {
		return "", false
	}
	parts := strings.SplitN(essence, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return essence, true
}

// requestIsCrossSite is the first check's predicate. It is false when
// Sec-Fetch-Site is same-origin or none; when it is same-site and the Origin
// host equals Host; when it is absent and the Origin host equals Host; and when
// neither header is present. It is true otherwise, including for an
// unrecognized Sec-Fetch-Site value.
func requestIsCrossSite(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site":
		return true
	case "same-origin", "none":
		return false
	case "same-site":
		// The same site may be another host: compare the Origin host.
		return !originMatchesHost(r.Header.Get("Origin"), r.Host)
	case "":
		// No Sec-Fetch-Site (an older browser or not a browser): use Origin.
		origin := r.Header.Get("Origin")
		if origin == "" {
			return false // neither header: not a browser
		}
		if strings.EqualFold(origin, "null") {
			return true
		}
		return !originMatchesHost(origin, r.Host)
	default:
		return true // an unrecognized Sec-Fetch-Site value
	}
}

// originMatchesHost reports whether the Origin header value names the same host
// as the request's Host header. "null", an empty value and a malformed Origin
// do not match. Only the host is compared, not the scheme, because a
// TLS-terminating proxy changes the request scheme. X-Forwarded-Host is not
// read, so the front proxy must pass the public Host header through.
func originMatchesHost(origin, host string) bool {
	if origin == "" || host == "" || strings.EqualFold(origin, "null") {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, host)
}
