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

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestResolveBundleLink: which links --check resolves, and to which path
// of the version (ptone/scion#4572).
func TestResolveBundleLink(t *testing.T) {
	for _, tc := range []struct {
		from, link, want string
		ok               bool
	}{
		{"index.html", "img/a.png", "img/a.png", true},
		{"docs/page.html", "../img/a.png", "img/a.png", true},
		{"docs/page.html", "./b.html", "docs/b.html", true},
		{"docs/page.html", "/css/s.css", "/css/s.css", true},
		{"index.html", "docs/", "docs/index.html", true},
		{"docs/page.html", "./", "docs/index.html", true},
		{"docs/page.html", "/", "/", true},
		{"index.html", "./", "index.html", true},
		{"index.html", "a%20b.html?x=1#y", "a b.html", true},
		{"index.html", "https://example.com/a", "", false},
		{"index.html", "//cdn.example.com/a.js", "", false},
		{"index.html", "mailto:a@example.com", "", false},
		{"index.html", "data:image/png;base64,AAAA", "", false},
		{"index.html", "javascript:void(0)", "", false},
		{"index.html", "#top", "", false},
		{"index.html", "?q=1", "", false},
		{"index.html", "../outside.html", "../outside.html", true},
		{"index.html", "a%zz.html", "a%zz.html", true},
		{"docs/page.html", "../../x.html", "../x.html", true},
	} {
		got, ok := resolveBundleLink(tc.from, tc.link)
		assert.Equal(t, tc.ok, ok, "%s -> %s", tc.from, tc.link)
		assert.Equal(t, tc.want, got, "%s -> %s", tc.from, tc.link)
	}
}

// TestRelativeLinks: href and src values are collected in document order,
// from start and self-closing tags, trimmed, and empty ones are skipped.
func TestRelativeLinks(t *testing.T) {
	doc := `<html><head><link rel=stylesheet href=" s.css "><script src="a.js"></script></head>` +
		`<body><img src="i.png"/><a href="">empty</a><a name="x">n</a><a href="p.html">p</a></body></html>`
	assert.Equal(t, []htmlLink{
		{attr: "href", target: "s.css"},
		{attr: "src", target: "a.js"},
		{attr: "src", target: "i.png"},
		{attr: "href", target: "p.html"},
	}, relativeLinks([]byte(doc)))
}

// TestLinkPresent: a link names a file of the version, or a folder of it
// holding index.html; a path that leaves the view never does.
func TestLinkPresent(t *testing.T) {
	present := map[string]bool{"a.html": true, "docs/index.html": true}
	assert.True(t, linkPresent(present, "a.html"))
	assert.True(t, linkPresent(present, "docs"))
	assert.True(t, linkPresent(present, "docs/index.html"))
	assert.False(t, linkPresent(present, "b.html"))
	assert.False(t, linkPresent(present, "/a.html"))
	assert.False(t, linkPresent(present, "../a.html"))
}

// TestIsHTMLMediaType: only text/html, with or without parameters.
func TestIsHTMLMediaType(t *testing.T) {
	assert.True(t, isHTMLMediaType("text/html"))
	assert.True(t, isHTMLMediaType("text/html; charset=utf-8"))
	assert.True(t, isHTMLMediaType("TEXT/HTML"))
	assert.False(t, isHTMLMediaType("text/plain"))
	assert.False(t, isHTMLMediaType("application/xhtml+xml"))
}
