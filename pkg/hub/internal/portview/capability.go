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

// Package portview holds the request-independent parts of the port view
// route (/api/v1/portview/<capability>/<path>, experiment hub.port_view):
// the capability, the navigation predicate and the preflight header rules.
// The hub's handler (pkg/hub/port_view.go) uses them. A capability names
// one user's access to one exposed port of one agent until an expiry; Mint
// and MintVersioned create one and Parse checks one.
//
// A capability has one of two forms:
//
//	v1.<agentID>.<port>.<userID>.<exp>.<sig>        (no key ID)
//	v2.<kid>.<agentID>.<port>.<userID>.<exp>.<sig>  (key ID kid)
//
// The hub's signing keys pick the form: the versioned form names the key
// generation that signed it, and only that generation checks it.
package portview

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Route is the path prefix of the port view route.
const Route = "/api/v1/portview/"

const (
	// version is the first field of a capability without a key ID.
	version = "v1"

	// versionKeyed is the first field of a capability with a key ID.
	versionKeyed = "v2"

	// sigDomain starts every signed message. The port view key is a key of
	// its own (not the artifact view key or any other hub key), and the
	// message carries this domain as well, so that a capability can never
	// be accepted by another route, nor another route's signature here.
	sigDomain = "port-view"

	// sigDomainKeyed starts every signed message of a capability with a
	// key ID; the key ID follows it, so a signature names its generation.
	sigDomainKeyed = "port-view-v2"

	// MaxSkew bounds how far beyond the configured lifetime an expiry may
	// lie. The hub never mints one; anything further out is refused.
	MaxSkew = time.Minute

	// fields is the number of dot-separated fields of a capability
	// without a key ID; one with a key ID has one more.
	fields = 6
)

// keyIDSyntax is the syntax of a key ID. A capability whose key ID does
// not match it is refused before any key is looked up.
var keyIDSyntax = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// Keys returns the keys that may have signed a capability carrying key ID
// kid ("" for a capability without one). An unknown key ID has none:
// Parse then refuses the capability and never tries another key.
type Keys func(kid string) [][]byte

// ErrInvalid is the only error Parse returns. Every way a capability can
// fail (shape, encoding, signature, expiry) is reported the same way.
var ErrInvalid = errors.New("invalid port view capability")

// sigEncoding is base64url without padding, refusing non-canonical
// trailing bits, so each signature has exactly one accepted spelling.
var sigEncoding = base64.RawURLEncoding.Strict()

// Grant is what a valid capability names.
type Grant struct {
	AgentID string
	Port    int
	UserID  string
	Expires time.Time
}

func mac(key []byte, agentID, port, userID, exp string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(sigDomain + "\n" + agentID + "\n" + port + "\n" + userID + "\n" + exp))
	return m.Sum(nil)
}

// macKeyed signs a capability with key ID kid: the message is
// "port-view-v2\n" + kid + "\n" + agentID, port, userID and exp, newline
// separated.
func macKeyed(key []byte, kid, agentID, port, userID, exp string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(sigDomainKeyed + "\n" + kid + "\n" + agentID + "\n" + port + "\n" + userID + "\n" + exp))
	return m.Sum(nil)
}

// Mint returns the capability without a key ID for userID's access to
// port of agentID until exp (whole seconds). The IDs must be canonical
// (see Parse).
func Mint(key []byte, agentID string, port int, userID string, exp time.Time) string {
	p := strconv.Itoa(port)
	e := strconv.FormatInt(exp.Unix(), 10)
	return version + "." + agentID + "." + p + "." + userID + "." + e + "." +
		sigEncoding.EncodeToString(mac(key, agentID, p, userID, e))
}

// MintVersioned is Mint for the key generation kid: the capability carries
// kid and its signature covers it. kid must match the key ID syntax.
func MintVersioned(key []byte, kid, agentID string, port int, userID string, exp time.Time) string {
	p := strconv.Itoa(port)
	e := strconv.FormatInt(exp.Unix(), 10)
	return versionKeyed + "." + kid + "." + agentID + "." + p + "." + userID + "." + e + "." +
		sigEncoding.EncodeToString(macKeyed(key, kid, agentID, p, userID, e))
}

// split returns the key ID ("" for none) and the five fields after it of
// a capability of either form, or ok false for any other shape: six
// fields starting with "v1", or seven starting with "v2" whose key ID
// matches the key ID syntax.
func split(capability string) (kid string, rest []string, ok bool) {
	parts := strings.Split(capability, ".")
	switch {
	case len(parts) == fields && parts[0] == version:
		return "", parts[1:], true
	case len(parts) == fields+1 && parts[0] == versionKeyed && keyIDSyntax.MatchString(parts[1]):
		return parts[1], parts[2:], true
	}
	return "", nil, false
}

// Parse checks capability against keys at now and returns what it names.
// It refuses, with ErrInvalid: nil keys; any shape but the two forms (six
// fields starting with "v1", or seven starting with "v2" with a key ID of
// the key ID syntax); an agent or user ID that is not a canonical
// (lower-case, hyphenated) UUID; a port or expiry that is not a decimal
// without sign or leading zero, or a port outside 1..65535; a signature
// that is not the canonical encoding or matches none of the keys that
// keys returns for the capability's key ID (none for an unknown one); an
// expiry that is not after now, or that lies more than ttl+MaxSkew after
// now.
func Parse(keys Keys, capability string, now time.Time, ttl time.Duration) (Grant, error) {
	if keys == nil {
		return Grant{}, ErrInvalid
	}
	kid, parts, ok := split(capability)
	if !ok {
		return Grant{}, ErrInvalid
	}
	agentID, portStr, userID, expStr, sigStr := parts[0], parts[1], parts[2], parts[3], parts[4]
	if !canonicalUUID(agentID) || !canonicalUUID(userID) {
		return Grant{}, ErrInvalid
	}
	port, ok := canonicalDecimal(portStr)
	if !ok || port < 1 || port > 65535 {
		return Grant{}, ErrInvalid
	}
	exp, ok := canonicalDecimal(expStr)
	if !ok || exp < 1 {
		return Grant{}, ErrInvalid
	}
	sig, err := sigEncoding.DecodeString(sigStr)
	if err != nil {
		return Grant{}, ErrInvalid
	}
	matched := false
	for _, key := range keys(kid) {
		if len(key) == 0 {
			continue
		}
		var want []byte
		if kid == "" {
			want = mac(key, agentID, portStr, userID, expStr)
		} else {
			want = macKeyed(key, kid, agentID, portStr, userID, expStr)
		}
		if hmac.Equal(sig, want) {
			matched = true
			break
		}
	}
	if !matched {
		return Grant{}, ErrInvalid
	}
	t := time.Unix(exp, 0)
	if !now.Before(t) || t.After(now.Add(ttl+MaxSkew)) {
		return Grant{}, ErrInvalid
	}
	return Grant{AgentID: agentID, Port: int(port), UserID: userID, Expires: t}, nil
}

// Target returns the agent ID and port fields of capability without
// checking its signature or expiry. The values are not trusted: they are
// only used to send a page navigation back to the agent port's stable
// link, which checks the caller itself.
func Target(capability string) (agentID string, port int, ok bool) {
	_, parts, ok := split(capability)
	if !ok || !canonicalUUID(parts[0]) {
		return "", 0, false
	}
	p, ok := canonicalDecimal(parts[1])
	if !ok || p < 1 || p > 65535 {
		return "", 0, false
	}
	return parts[0], int(p), true
}

// CanonicalID reports whether s may be used as an agent or user ID in a
// capability: a UUID in its canonical (lower-case, hyphenated) spelling.
func CanonicalID(s string) bool { return canonicalUUID(s) }

// canonicalUUID reports whether s is a UUID in its canonical spelling.
func canonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s
}

// canonicalDecimal parses s as a non-negative decimal of at most 18
// digits with no sign and no leading zero ("0" itself is accepted).
func canonicalDecimal(s string) (int64, bool) {
	if s == "" || len(s) > 18 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// IsNavigation reports whether r looks like a page navigation:
// Sec-Fetch-Mode "navigate", or, when the browser sent no Sec-Fetch-Mode,
// an Accept header that includes text/html. The caller chooses these
// headers, so the answer only selects a product behaviour (which link the
// browser is sent to); it is never an access decision.
func IsNavigation(r *http.Request) bool {
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// preflightMethods are the methods a preflight may be answered for.
var preflightMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
}

// PreflightMethod returns the value of Access-Control-Allow-Methods for a
// preflight that asks for method: method itself when it is GET, HEAD,
// POST, PUT, PATCH or DELETE, otherwise "" (the header is not sent).
func PreflightMethod(method string) string {
	if preflightMethods[method] {
		return method
	}
	return ""
}

// PreflightHeaders returns the value of Access-Control-Allow-Headers for a
// preflight whose Access-Control-Request-Headers is requested: the
// requested names, lower-cased, except authorization, cookie and any name
// that is not an HTTP token. "" means the header is not sent.
func PreflightHeaders(requested string) string {
	var out []string
	for _, name := range strings.Split(requested, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || name == "authorization" || name == "cookie" || !isToken(name) {
			continue
		}
		out = append(out, name)
	}
	return strings.Join(out, ", ")
}

// isToken reports whether s is an HTTP token (RFC 9110 section 5.6.2).
func isToken(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return s != ""
}
