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
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mobileFrameStartMarker and mobileFrameEndMarker are the literal comment
// markers that bound the critical-CSS block which must stay identical
// between pkg/hub/web.go's spaShellTemplate and web/index.html. The parity
// check (TestSPAShellIndexHTMLParity) compares both sides as raw source —
// spaShellTemplate is a package-level string this test can read directly,
// with its comments intact, so there is no need to render it through
// html/template first. Comparing raw source on both sides means the literal
// markers bound the exact same region on both sides, with no separate
// per-side anchor to keep in sync.
//
// Both markers include the comment's own opening "/*": cutting at
// "mobile-frame:start"/"mobile-frame:end" alone would exclude (for the
// start) or leave dangling (for the end) that "/*", so normalizeCSS's
// comment stripper either can't match the marker comment as a complete
// comment at all, or is left with no matching "*/" within the extracted
// range — either way, the marker comment's own wording (which legitimately
// differs between the two files, each pointing at the other) would leak
// through as literal, uncaught text instead of being stripped.
const (
	mobileFrameStartMarker = "/* mobile-frame:start"
	mobileFrameEndMarker   = "/* mobile-frame:end"
)

// renderSPAShell executes spaShellTemplate with placeholder data and returns
// the resulting HTML, without needing a running WebServer or HTTP request.
func renderSPAShell(t *testing.T) string {
	t.Helper()
	ws := NewWebServer(WebServerConfig{})
	require.NotNil(t, ws.shellTmpl, "spaShellTemplate must parse")
	var buf bytes.Buffer
	require.NoError(t, ws.shellTmpl.Execute(&buf, spaShellData{ShoelaceVersion: "0.0.0-test"}))
	return buf.String()
}

// extractBetween returns the substring from the first occurrence of start up
// to (not including) the first occurrence of end found after start.
func extractBetween(t *testing.T, html, start, end string) string {
	t.Helper()
	startIdx := strings.Index(html, start)
	require.NotEqual(t, -1, startIdx, "start marker %q not found", start)
	endIdx := strings.Index(html[startIdx:], end)
	require.NotEqual(t, -1, endIdx, "end marker %q not found after start", end)
	return html[startIdx : startIdx+endIdx]
}

// mobileFrameRenderedStartAnchor and mobileFrameRenderedNextRule bound a
// generous (not exact) slice of the *rendered* shell for
// TestSPAShellMobileFrameCSS's `Contains`/`NotContains` checks, which only
// need to land somewhere inside the block, not match it exactly — unlike
// the parity check, this test never compares the slice against anything, so
// a loose boundary is fine. html/template strips comments from the rendered
// output, so this can't anchor on "mobile-frame:start"/"mobile-frame:end"
// the way the raw-source parity check does; it anchors on content that
// survives rendering instead.
const (
	mobileFrameRenderedStartAnchor = "--scion-app-height: 100vh;"
	mobileFrameRenderedNextRule    = "scion-app:not(:defined)"
)

// extractRenderedFrameBlock returns a generous slice of the rendered shell
// containing the mobile-frame block, for content (`Contains`) assertions
// only. See mobileFrameRenderedStartAnchor's doc comment for why this is
// separate from the raw-source extraction the parity check uses.
func extractRenderedFrameBlock(t *testing.T, html string) string {
	t.Helper()
	return extractBetween(t, html, mobileFrameRenderedStartAnchor, mobileFrameRenderedNextRule)
}

// cssCommentRE matches C-style CSS comments, used to strip them before
// comparison for two different reasons depending on which test uses it. In
// TestSPAShellMobileFrameCSS (rendered shell only) stripping is a harmless
// no-op, because html/template has already dropped comments from the
// rendered output by the time this runs. In TestSPAShellIndexHTMLParity
// (raw source on both sides) stripping matters for real: the two
// mobile-frame:start/:end marker comments are deliberately worded
// differently (each names the other file), and any other inner comments
// must not count as a rule difference either.
var cssCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)

// normalizeCSS strips comments and collapses every run of whitespace
// (including newlines) to a single space, so the comparison is insensitive
// to comment stripping, to indentation, and to how a multi-selector rule
// wraps across lines — while still catching any real difference in the
// rules themselves (a changed property, value, or selector always changes a
// token).
func normalizeCSS(block string) string {
	block = cssCommentRE.ReplaceAllString(block, "")
	return strings.Join(strings.Fields(block), " ")
}

// wantViewportMetaContent is the exact viewport meta content both page
// templates must carry: viewport-fit=cover lets the bars run edge to edge on
// notched and home-indicator devices (the safe-area-inset padding that goes
// with it lives in the header, composer, chat panels and app-shell content),
// and interactive-widget=resizes-content makes Android shrink the layout
// viewport for the on-screen keyboard. Pinch zoom must stay available, so
// there is never a maximum-scale or user-scalable.
const wantViewportMetaContent = "width=device-width, initial-scale=1, viewport-fit=cover, interactive-widget=resizes-content"

// viewportMetaRE captures the content attribute of the viewport meta tag.
var viewportMetaRE = regexp.MustCompile(`<meta name="viewport" content="([^"]*)"`)

// viewportMetaContents returns the content attribute of every viewport meta
// tag in html, so a test can catch a second, conflicting tag as well as a
// wrong value.
func viewportMetaContents(html string) []string {
	var contents []string
	for _, m := range viewportMetaRE.FindAllStringSubmatch(html, -1) {
		contents = append(contents, m[1])
	}
	return contents
}

func TestSPAShellViewportMeta(t *testing.T) {
	html := renderSPAShell(t)
	assert.Equal(t, []string{wantViewportMetaContent}, viewportMetaContents(html),
		"the SPA shell must carry exactly one viewport meta with the expected content")
	assert.NotContains(t, html, "maximum-scale",
		"viewport meta must never disable pinch zoom")
	assert.NotContains(t, html, "user-scalable",
		"viewport meta must never disable pinch zoom")
}

func TestSPAShellViewportMetaMatchesIndexHTML(t *testing.T) {
	indexBytes, err := os.ReadFile("../../web/index.html")
	require.NoError(t, err, "reading web/index.html")
	indexHTML := string(indexBytes)

	assert.Equal(t, []string{wantViewportMetaContent}, viewportMetaContents(indexHTML),
		"web/index.html must carry exactly one viewport meta with the expected content")
	assert.Equal(t, viewportMetaContents(renderSPAShell(t)), viewportMetaContents(indexHTML),
		"pkg/hub/web.go and web/index.html must carry the same viewport meta")
	assert.NotContains(t, indexHTML, "maximum-scale")
	assert.NotContains(t, indexHTML, "user-scalable")
}

func TestSPAShellMobileFrameCSS(t *testing.T) {
	html := renderSPAShell(t)
	block := normalizeCSS(extractRenderedFrameBlock(t, html))

	assert.Contains(t, block, "--scion-app-height: 100vh;")
	assert.Contains(t, block, "@supports (height: 100dvh)")
	assert.Contains(t, block, "overscroll-behavior: none;")
	assert.Contains(t, block, "html.scion-app-frame")
	assert.Contains(t, block, "height: 100%; min-height: 0;")

	// Touch targets: no double-tap zoom (pinch stays available), and every
	// Shoelace input/textarea computes at 16px or more on a coarse pointer
	// so focusing one never triggers iOS/Android focus-zoom. The selector
	// is "html:root", not a bare ":root": Shoelace's own theme sets these
	// same custom properties on a selector list that includes a bare
	// ":root" (light.css/dark.css) at the same specificity, so a bare
	// ":root" here would depend on stylesheet load order to win. Asserting
	// the raised-specificity selector by name catches a regression back to
	// the order-dependent form, which `Contains` on the property/value
	// pair alone would not.
	assert.Contains(t, block, "touch-action: manipulation;")
	assert.Contains(t, block, "@media (pointer: coarse)")
	assert.Contains(t, block, "html:root {")
	assert.Contains(t, block, "--sl-input-font-size-small: 16px;")
	assert.Contains(t, block, "--sl-input-font-size-medium: 16px;")
}

// TestSPAShellIndexHTMLParity guards the requirement that web.go and
// web/index.html change their critical mobile-frame CSS together (the
// viewport meta has its own check,
// TestSPAShellViewportMetaMatchesIndexHTML). It reads web/index.html
// directly off disk relative to this package, following the existing
// convention in permission_registry_test.go.
//
// The comparison reads spaShellTemplate as raw source (a package-level
// string, not rendered through html/template) and extracts both sides
// between the literal mobile-frame:start/mobile-frame:end markers, so the
// two sides use the exact same boundaries rather than a separate anchor per
// side. A change anywhere between the markers — before the first rule,
// inside an existing rule, between two rules, or after the last one — on
// only one side fails this test.
// TestMobileFrameBlockExtraction_CatchesAOneSidedChange proves that
// directly against synthetic input, covering all of those positions.
func TestSPAShellIndexHTMLParity(t *testing.T) {
	indexBytes, err := os.ReadFile("../../web/index.html")
	require.NoError(t, err, "reading web/index.html")
	indexHTML := string(indexBytes)

	shellBlock := normalizeCSS(extractBetween(t, spaShellTemplate, mobileFrameStartMarker, mobileFrameEndMarker))
	indexBlock := normalizeCSS(extractBetween(t, indexHTML, mobileFrameStartMarker, mobileFrameEndMarker))
	assert.Equal(t, shellBlock, indexBlock,
		"critical mobile-frame CSS must stay identical (modulo comments and formatting) between pkg/hub/web.go and web/index.html")
}

// mobileFrameSyntheticBlock is a small stand-in for the real mobile-frame
// block, with three placeholders marking every position a one-sided change
// could land: before the first rule, between the block's two rules, and
// after the last rule. buildMobileFrameSyntheticBlock fills them in (empty
// string for "no change here").
const mobileFrameSyntheticBlock = "/* mobile-frame:start */\n@@PREPEND@@:root {\n  --scion-app-height: 100vh;\n}\n@@MIDDLE@@html.scion-app-frame, html.scion-app-frame body { overflow: hidden; height: 100%; }\n@@APPEND@@/* mobile-frame:end */"

func buildMobileFrameSyntheticBlock(prepend, middle, appendRule string) string {
	s := strings.ReplaceAll(mobileFrameSyntheticBlock, "@@PREPEND@@", prepend)
	s = strings.ReplaceAll(s, "@@MIDDLE@@", middle)
	return strings.ReplaceAll(s, "@@APPEND@@", appendRule)
}

// TestMobileFrameBlockExtraction_CatchesAOneSidedChange is a regression test
// for the extraction logic TestSPAShellIndexHTMLParity relies on, run
// against small synthetic strings rather than the real template and file.
// It proves a one-sided change changes the normalized comparison no matter
// where in the block it lands — before the first rule, between two rules,
// after the last rule, or a changed selector on the first rule — and that
// two independently-built but identical blocks still compare equal.
func TestMobileFrameBlockExtraction_CatchesAOneSidedChange(t *testing.T) {
	const extraRule = "@media (pointer: coarse) { html { touch-action: manipulation; } }\n"

	base := buildMobileFrameSyntheticBlock("", "", "")
	baseBlock := normalizeCSS(extractBetween(t, base, mobileFrameStartMarker, mobileFrameEndMarker))

	cases := map[string]string{
		"a rule prepended right after the start marker": buildMobileFrameSyntheticBlock(extraRule, "", ""),
		"a rule inserted between the block's two rules": buildMobileFrameSyntheticBlock("", extraRule, ""),
		"a rule appended right before the end marker":   buildMobileFrameSyntheticBlock("", "", extraRule),
	}
	for name, mutated := range cases {
		t.Run(name, func(t *testing.T) {
			mutatedBlock := normalizeCSS(extractBetween(t, mutated, mobileFrameStartMarker, mobileFrameEndMarker))
			assert.NotEqual(t, baseBlock, mutatedBlock, "a one-sided change must be caught")
		})
	}

	t.Run("the first rule's selector changed", func(t *testing.T) {
		changed := strings.Replace(base, ":root {", "html {", 1)
		changedBlock := normalizeCSS(extractBetween(t, changed, mobileFrameStartMarker, mobileFrameEndMarker))
		assert.NotEqual(t, baseBlock, changedBlock, "a changed first selector must be caught")
	})

	t.Run("two independently-built identical blocks still match", func(t *testing.T) {
		otherBlock := normalizeCSS(extractBetween(t, buildMobileFrameSyntheticBlock("", "", ""), mobileFrameStartMarker, mobileFrameEndMarker))
		assert.Equal(t, baseBlock, otherBlock, "sanity check: identical input must compare equal")
	})
}
