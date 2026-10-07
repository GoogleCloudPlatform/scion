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

import "strings"

// normalizeImageURL turns a kept candidate into the URL the hub fetches:
// it decodes the candidate the way the browser does for its kind (character
// references in an attribute value, or in a markdown destination), applies
// the parts of the WHATWG URL parser that change which resource an http(s)
// URL names (tabs and line breaks removed, any run of '/' or '\' after the
// scheme read as "//", '\' read as '/' before the query), and accepts the
// result only if it is in a plain subset of URL syntax that Go's URL parser
// reads the same way: an http or https scheme, a host of letters, digits,
// '.' and '-', an optional numeric port, no userinfo, and a path, query and
// fragment of unreserved characters, sub-delimiters (except parentheses),
// ':', '@', '/', '?' and valid percent-escapes. Anything else is refused,
// and the image shows its "not fetched" placeholder.
//
// It runs only on the candidates the scan kept, at most once each.
func normalizeImageURL(raw string, kind int) (string, bool) {
	var steps int
	return normalizeCounted(raw, kind, &steps)
}

// normalizeCounted is normalizeImageURL that adds the bytes it examines to
// *steps. Each step reads its input once, so the count is a fixed multiple
// of len(raw) at most.
func normalizeCounted(raw string, kind int, steps *int) (string, bool) {
	// This check is the single bound on the work and memory of normalizing
	// one candidate: a reference definition's destination is read without
	// a cap of its own. It is defended by TestExtractImageURLsNormalizationWork
	// and TestExtractImageURLsAllocBound, which fail without it. A second,
	// redundant cap elsewhere would leave those tests unable to fail.
	if len(raw) > maxImageURLBytes {
		return "", false
	}
	var decoded string
	var ok bool
	if kind == fromAttribute {
		decoded, ok = decodeAttributeRefs(raw, steps)
	} else {
		decoded, ok = decodeMarkdownRefs(raw, steps)
	}
	if !ok {
		return "", false
	}
	return canonicalHTTPURL(decoded, steps)
}

// legacyRefs are the named character references the HTML tokenizer
// decodes without a trailing ';'.
var legacyRefs = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range strings.Fields(`AElig AMP Aacute Acirc Agrave Aring Atilde Auml COPY Ccedil ETH
		Eacute Ecirc Egrave Euml GT Iacute Icirc Igrave Iuml LT Ntilde Oacute Ocirc Ograve Oslash Otilde
		Ouml QUOT REG THORN Uacute Ucirc Ugrave Uuml Yacute aacute acirc acute aelig agrave amp aring
		atilde auml brvbar ccedil cedil cent copy curren deg divide eacute ecirc egrave eth euml frac12
		frac14 frac34 gt iacute icirc iexcl igrave iquest iuml laquo lt macr micro middot nbsp not ntilde
		oacute ocirc ograve ordf ordm oslash otilde ouml para plusmn pound quot raquo reg sect shy sup1
		sup2 sup3 szlig thorn times uacute ucirc ugrave uml uuml yacute yen yuml`) {
		m[n] = true
	}
	return m
}()

// asciiRefs are the named references whose value is printable ASCII and
// that a URL plausibly holds.
var asciiRefs = map[string]byte{
	"amp": '&', "AMP": '&', "lt": '<', "LT": '<', "gt": '>', "GT": '>',
	"quot": '"', "QUOT": '"', "apos": '\'',
}

func isASCIIAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// decodeAttributeRefs decodes character references in an attribute value
// as the HTML tokenizer does in an attribute. A reference whose value is
// not printable ASCII (or tab or line break, which the URL parser removes)
// refuses the value, as does a named reference with ';' that is not one of
// asciiRefs: without the full table its value is unknown.
func decodeAttributeRefs(v string, steps *int) (string, bool) {
	*steps += len(v)
	if strings.IndexByte(v, '&') < 0 {
		return v, true
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); {
		c := v[i]
		if c != '&' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 < len(v) && v[i+1] == '#' {
			r, n, ok := numericRef(v[i+2:])
			if !ok {
				return "", false
			}
			if n == 0 { // "&#" without digits stays as written
				b.WriteString("&#")
				i += 2
				continue
			}
			b.WriteByte(r)
			i += 2 + n
			continue
		}
		j := i + 1
		for j < len(v) && isASCIIAlnum(v[j]) {
			j++
		}
		name := v[i+1 : j]
		switch {
		case name == "":
			b.WriteByte('&')
			i++
		case j < len(v) && v[j] == ';':
			r, ok := asciiRefs[name]
			if !ok {
				return "", false
			}
			b.WriteByte(r)
			i = j + 1
		case legacyRefs[name] && (j >= len(v) || v[j] != '='):
			// Decoded without ';' when the whole run is a legacy name
			// and '=' does not follow (a shorter match is followed by an
			// alphanumeric, which keeps it as written in an attribute).
			r, ok := asciiRefs[name]
			if !ok {
				return "", false
			}
			b.WriteByte(r)
			i = j
		default:
			b.WriteString(v[i:j])
			i = j
		}
	}
	return b.String(), true
}

// decodeMarkdownRefs decodes the references a markdown destination may
// hold. Only &amp; is decoded; any other reference with ';', and any
// numeric reference, refuses the destination (renderers differ on them).
// An '&' that starts no reference stays as written.
func decodeMarkdownRefs(v string, steps *int) (string, bool) {
	*steps += len(v)
	if strings.IndexByte(v, '&') < 0 {
		return v, true
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); {
		c := v[i]
		if c != '&' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 < len(v) && v[i+1] == '#' {
			return "", false
		}
		j := i + 1
		for j < len(v) && isASCIIAlnum(v[j]) {
			j++
		}
		if j < len(v) && v[j] == ';' && j > i+1 {
			if v[i+1:j] != "amp" {
				return "", false
			}
			b.WriteByte('&')
			i = j + 1
			continue
		}
		b.WriteString(v[i:j])
		i = j
	}
	return b.String(), true
}

// numericRef reads the digits of a numeric reference after "&#" (with an
// optional x for hex and an optional ';'). n is the bytes consumed, 0 when
// no digit follows. ok is false when the value is not printable ASCII,
// tab, line feed or carriage return.
func numericRef(s string) (byte, int, bool) {
	i, base := 0, 10
	if i < len(s) && (s[i] == 'x' || s[i] == 'X') {
		i, base = 1, 16
	}
	start := i
	val := 0
	for i < len(s) {
		d := hexDigit(s[i])
		if d < 0 || d >= base {
			break
		}
		if val < 0x110000 {
			val = val*base + d
		}
		i++
	}
	if i == start {
		return 0, 0, true
	}
	if i < len(s) && s[i] == ';' {
		i++
	}
	switch {
	case val >= 0x21 && val <= 0x7e, val == '\t', val == '\n', val == '\r':
		return byte(val), i, true
	}
	return 0, 0, false
}

func hexDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// canonicalHTTPURL applies the URL parser steps described at
// normalizeImageURL and checks the result. It returns the URL with a lower
// case scheme followed by "://".
func canonicalHTTPURL(v string, steps *int) (string, bool) {
	v = trimURLSpace(v)
	*steps += 2 * len(v)
	if strings.ContainsAny(v, "\t\n\r") {
		// One buffer no longer than v, written once.
		var b strings.Builder
		b.Grow(len(v))
		for i := 0; i < len(v); i++ {
			if c := v[i]; c != '\t' && c != '\n' && c != '\r' {
				b.WriteByte(c)
			}
		}
		v = b.String()
	}
	var scheme string
	switch {
	case hasPrefixFold(v, "https:"):
		scheme, v = "https", v[len("https:"):]
	case hasPrefixFold(v, "http:"):
		scheme, v = "http", v[len("http:"):]
	default:
		return "", false
	}
	for len(v) > 0 && (v[0] == '/' || v[0] == '\\') {
		v = v[1:]
	}
	end := strings.IndexAny(v, "/\\?#")
	if end < 0 {
		end = len(v)
	}
	authority, rest := v[:end], v[end:]
	if !validAuthority(authority) {
		return "", false
	}
	var b strings.Builder
	b.Grow(len(scheme) + 3 + len(v))
	b.WriteString(scheme)
	b.WriteString("://")
	// Host names are case-insensitive; the lower case form is the one the
	// browser's URL parser produces.
	b.WriteString(strings.ToLower(authority))
	inQuery, fragment := false, false
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case c == '\\' && !inQuery:
			c = '/'
		case c == '?' && !inQuery:
			inQuery = true
		case c == '#':
			if fragment {
				return "", false
			}
			inQuery, fragment = true, true
		case c == '%':
			if i+2 >= len(rest) || hexDigit(rest[i+1]) < 0 || hexDigit(rest[i+2]) < 0 {
				return "", false
			}
		case !urlRestByte(c):
			return "", false
		}
		b.WriteByte(c)
	}
	if b.Len() > maxImageURLBytes {
		return "", false
	}
	return b.String(), true
}

// validAuthority accepts host[:port] with a host of letters, digits, '.'
// and '-' (at most 253 bytes, not starting with '.') and a port of one to
// five digits no larger than 65535. Userinfo, IP literals in brackets and
// anything else are refused.
func validAuthority(a string) bool {
	host, port, hasPort := strings.Cut(a, ":")
	if host == "" || len(host) > 253 || host[0] == '.' {
		return false
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		if !isASCIIAlnum(c) && c != '.' && c != '-' {
			return false
		}
	}
	if !hasPort {
		return true
	}
	if port == "" || len(port) > 5 {
		return false
	}
	n := 0
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
		n = n*10 + int(port[i]-'0')
	}
	return n <= 65535
}

// urlRestByte reports whether c may appear as itself in the path, query or
// fragment of a URL the hub fetches.
func urlRestByte(c byte) bool {
	if isASCIIAlnum(c) {
		return true
	}
	switch c {
	case '-', '.', '_', '~', '!', '$', '&', '\'', '*', '+', ',', ';', '=', ':', '@', '/', '?':
		return true
	}
	return false
}
