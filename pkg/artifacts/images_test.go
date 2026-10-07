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
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExtractImageURLs(t *testing.T) {
	for _, tc := range []struct {
		name string
		md   string
		want []string
	}{
		{"inline", "![a](https://img.example/a.png)", []string{"https://img.example/a.png"}},
		{"inline with title", `![a](https://img.example/a.png "t") ![b](http://img.example/b.png 'x')`, []string{"https://img.example/a.png", "http://img.example/b.png"}},
		{"angle destination", "![a](<https://img.example/a.png>)", []string{"https://img.example/a.png"}},
		{"space after paren", "![a](  https://img.example/a.png)", []string{"https://img.example/a.png"}},
		{"nested alt", "![a [b] c](https://img.example/a.png)", []string{"https://img.example/a.png"}},
		{"link is not an image", "[a](https://example.com/page) ![b](https://img.example/b.png)", []string{"https://img.example/b.png"}},
		{"image inside a link", "[![b](https://img.example/b.png)](https://example.com/)", []string{"https://img.example/b.png"}},
		{"escaped bang", `\![a](https://img.example/a.png)`, nil},
		{"escaped bracket", `!\[a](https://img.example/a.png)`, nil},
		{"relative", "![a](img/a.png) ![b](/abs.png) ![c](//cdn.example/c.png)", nil},
		{"other schemes", "![a](data:image/png;base64,AAAA) ![b](javascript:alert(1)) ![c](ftp://x/c.png)", nil},
		{"uppercase scheme", "![a](HTTPS://img.example/a.png)", []string{"https://img.example/a.png"}},
		{"dedupe and order", "![a](https://i.example/2.png) ![b](https://i.example/1.png) ![c](https://i.example/2.png)", []string{"https://i.example/2.png", "https://i.example/1.png"}},
		{"parens in URL are not kept", "![a](https://img.example/a_(1).png)", nil},
		{"unterminated", "![a](https://img.example/a.png", nil},
		{"amp entity", "![a](https://img.example/a.png?w=1&amp;h=2)", []string{"https://img.example/a.png?w=1&h=2"}},
		{"raw ampersand", "![a](https://img.example/a.png?w=1&h=2)", []string{"https://img.example/a.png?w=1&h=2"}},
		{"other entity refused", "![a](https://img.example/a&copy;.png)", nil},
		{"numeric entity refused", "![a](https://img.example/a&#47;b.png)", nil},
		{"reference full", "![a][logo]\n\n[logo]: https://img.example/logo.png", nil},
		{"reference collapsed", "![Logo][]\n\n[logo]: https://img.example/logo.png", nil},
		{"reference shortcut", "![logo]\n\n[LOGO]: <https://img.example/logo.png> \"title\"", nil},
		{"label whitespace folded", "![a][My   Logo]\n\n[my logo]: https://img.example/logo.png", nil},
		{"definition on next line", "![a][x]\n\n[x]:\n  https://img.example/x.png", nil},
		{"first definition wins", "![a][x]\n\n[x]: https://img.example/1.png\n[x]: https://img.example/2.png", nil},
		{"unused definition", "[x]: https://img.example/x.png\n[y](https://example.com)", nil},
		{"link reference not an image", "[a][x]\n\n[x]: https://img.example/x.png", nil},
		{"definition indented four", "![a][x]\n\n    [x]: https://img.example/x.png", nil},
		{"reference order", "![a][x] ![b](https://img.example/b.png)\n\n[x]: https://img.example/x.png", []string{"https://img.example/b.png"}},
		{"img tag", `<img src="https://img.example/a.png" alt="a">`, []string{"https://img.example/a.png"}},
		{"img tag inline", `text <IMG alt=x SRC='https://img.example/a.png'> more`, []string{"https://img.example/a.png"}},
		{"img tag unquoted", `<img src=https://img.example/a.png>`, []string{"https://img.example/a.png"}},
		{"img tag self closing", `<img src="https://img.example/a.png"/>`, []string{"https://img.example/a.png"}},
		{"img tag first src wins", `<img src="https://img.example/1.png" src="https://img.example/2.png">`, []string{"https://img.example/1.png"}},
		{"img tag data-src is not src", `<img data-src="https://img.example/1.png">`, nil},
		{"imgx is not img", `<imgx src="https://img.example/1.png">`, nil},
		{"img tag unterminated quote", `<img src="https://img.example/1.png>`, nil},
		{"img tag never closed", `<img src="https://img.example/1.png" `, nil},
		{"img tag with lt inside", `<img src="https://img.example/1.png" <b>`, nil},
		{"img src attribute rules", `<img src="https://img.example/a.png?a=1&amp;b=2&copy=3">`, []string{"https://img.example/a.png?a=1&b=2&copy=3"}},
		{"img src legacy reference decoded", `<img src="https://img.example/a.png?x=&copy">`, nil},
		{"img src numeric reference", `<img src="https://img.example/a&#x2F;b.png">`, []string{"https://img.example/a/b.png"}},
		{"img src tab in scheme", "<img src=\"ht\ttps://img.example/a.png\">", []string{"https://img.example/a.png"}},
		{"img src backslashes", `<img src="https:\\img.example\a.png">`, []string{"https://img.example/a.png"}},
		{"img src no slashes", `<img src="https:img.example/a.png">`, []string{"https://img.example/a.png"}},
		{"img src padded", `<img src="  https://img.example/a.png  ">`, []string{"https://img.example/a.png"}},
	} {
		got := extractImageURLs(tc.md, 128).urls
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// normalizeCase is one row of the remote image URL normalization table,
// shared with the web renderer's tests through
// testdata/remote_image_urls.json.
type normalizeCase struct {
	Raw  string `json:"raw"`
	Kind string `json:"kind"` // "markdown" or "attribute"
	Want string `json:"want"` // "" = refused
}

func kindName(k int) string {
	if k == fromAttribute {
		return "attribute"
	}
	return "markdown"
}

var normalizeCases = func() []normalizeCase {
	var out []normalizeCase
	for _, tc := range []struct {
		raw  string
		kind int
		want string
	}{

		{"https://h.example/a.png", fromMarkdown, "https://h.example/a.png"},
		{"HTTPS://IMG.Example:443/A.png", fromMarkdown, "https://img.example:443/A.png"},
		{"https://h.example:8443/a.png?x=1#f", fromMarkdown, "https://h.example:8443/a.png?x=1#f"},
		{"https://h.example/%E2%9C%93.png", fromMarkdown, "https://h.example/%E2%9C%93.png"},
		{"https://h.example/a%2.png", fromMarkdown, ""},
		{"https://user@h.example/a.png", fromMarkdown, ""},
		{"https://user:pw@h.example/a.png", fromMarkdown, ""},
		{"https://[::1]/a.png", fromMarkdown, ""},
		{"https://h.example:99999/a.png", fromMarkdown, ""},
		{"https://h.example:/a.png", fromMarkdown, ""},
		{"https:///a.png", fromMarkdown, "https://a.png"}, // the URL parser skips any run of slashes
		{"https://.h/a.png", fromMarkdown, ""},
		{"https://h_x.example/a.png", fromMarkdown, ""},
		{"https://h.example/a b.png", fromMarkdown, ""},
		{"https://h.example/a\"b.png", fromMarkdown, ""},
		{"https://h.example/a.png#x#y", fromMarkdown, ""},
		{"https://h.example/" + strings.Repeat("a", maxImageURLBytes), fromMarkdown, ""},
		// Within the input cap, but the normalized URL ("https://" added)
		// is over it.
		{"https:h.example/" + strings.Repeat("a", maxImageURLBytes-len("https:h.example/")-1), fromAttribute, ""},
		{"ftp://h.example/a.png", fromMarkdown, ""},
		{"https://h.example/a\\b.png", fromAttribute, "https://h.example/a/b.png"},
		{"https://h.example/a.png?q=\\x", fromAttribute, ""},
		{"https://h.example\\a.png", fromAttribute, "https://h.example/a.png"},
		{"https:/\\/h.example/a.png", fromAttribute, "https://h.example/a.png"},
		{"h\nttps://h.example/a.png", fromAttribute, "https://h.example/a.png"},
		{"https://h.ex\tample/a.png", fromAttribute, "https://h.example/a.png"},
		{"https://h.example/a.png?a=1&b=2", fromAttribute, "https://h.example/a.png?a=1&b=2"},
		{"https://h.example/a.png?a=1&amp;b=2", fromAttribute, "https://h.example/a.png?a=1&b=2"},
		{"https://h.example/a.png?a=1&amp=2", fromAttribute, "https://h.example/a.png?a=1&amp=2"},
		{"https://h.example/a.png?a=1&amp", fromAttribute, "https://h.example/a.png?a=1&"},
		{"https://h.example/a.png?a=1&ampx", fromAttribute, "https://h.example/a.png?a=1&ampx"},
		{"https://h.example/a.png?&copy", fromAttribute, ""},
		{"https://h.example/a.png?&copy;", fromAttribute, ""},
		{"https://h.example/a.png?&copy=1", fromAttribute, "https://h.example/a.png?&copy=1"},
		{"https://h.example/a.png?&notanentity;", fromAttribute, ""},
		{"https://h.example/a&#47;b.png", fromAttribute, "https://h.example/a/b.png"},
		{"https://h.example/a&#47b.png", fromAttribute, "https://h.example/a/b.png"},
		{"https://h.example/a&#9;b.png", fromAttribute, "https://h.example/ab.png"},
		{"https://h.example/a&#0;b.png", fromAttribute, ""},
		{"https://h.example/a&#233;.png", fromAttribute, ""},
		{"https://h.example/a&#.png", fromAttribute, "https://h.example/a&#.png"}, // "&#" with no digit stays; '#' starts the fragment
		{"https://h.example/a.png?a&amp;b", fromMarkdown, "https://h.example/a.png?a&b"},
		{"https://h.example/a.png?a&lt;b", fromMarkdown, ""},
		{"https://h.example/a.png?a&#38;b", fromMarkdown, ""},
		{"https://h.example/a.png?a&b", fromMarkdown, "https://h.example/a.png?a&b"},
	} {
		out = append(out, normalizeCase{Raw: tc.raw, Kind: kindName(tc.kind), Want: tc.want})
	}
	return out
}()

const normalizeFixture = "testdata/remote_image_urls.json"

// TestNormalizeImageURLFixture keeps the shared fixture equal to the
// table. Regenerate it with SCION_UPDATE_FIXTURES=1.
func TestNormalizeImageURLFixture(t *testing.T) {
	want, err := json.MarshalIndent(normalizeCases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if os.Getenv("SCION_UPDATE_FIXTURES") != "" {
		if err := os.WriteFile(normalizeFixture, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(normalizeFixture)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date (%v); run with SCION_UPDATE_FIXTURES=1", normalizeFixture, err)
	}
}

// TestNormalizeImageURL checks the decoding and the accepted URL subset,
// and that every accepted URL parses in Go to the same scheme and host
// with no userinfo.
func TestNormalizeImageURL(t *testing.T) {
	for _, tc := range normalizeCases {
		kind := fromMarkdown
		if tc.Kind == "attribute" {
			kind = fromAttribute
		}
		got, ok := normalizeImageURL(tc.Raw, kind)
		if !ok {
			got = ""
		}
		if got != tc.Want {
			t.Errorf("normalize(%q, %s) = %q, want %q", tc.Raw, tc.Kind, got, tc.Want)
			continue
		}
		if got == "" {
			continue
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Errorf("%q does not parse: %v", got, err)
			continue
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Hostname() == "" || strings.ContainsAny(u.Host, "@[]") {
			t.Errorf("%q parses to scheme %q host %q user %v", got, u.Scheme, u.Host, u.User)
		}
	}
}

func TestExtractImageURLsLimit(t *testing.T) {
	for _, n := range []int{3, 4, 5, 50} {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "![](https://img.example/%d.png)\n", i)
		}
		ex := extractImageURLs(b.String(), 4)
		if want := min(n, 4); len(ex.urls) != want || ex.full != (n >= 4) {
			t.Errorf("%d URLs, limit 4: got %d (full %v)", n, len(ex.urls), ex.full)
		}
		if ex.urls[0] != "https://img.example/0.png" {
			t.Errorf("first URL %q", ex.urls[0])
		}
	}
	if ex := extractImageURLs("![](https://img.example/a.png)", 0); len(ex.urls) != 0 {
		t.Errorf("limit 0 found %v", ex.urls)
	}
}

// TestExtractImageURLsStatedChecks has one case per check the scan states,
// each failing when that check is removed.
func TestExtractImageURLsStatedChecks(t *testing.T) {
	for name, tc := range map[string]struct {
		md    string
		limit int
		want  []string
	}{
		"markdown inside an img attribute is not read":    {`<img alt="![a](https://img.example/alt.png)" src="https://img.example/src.png">`, 8, []string{"https://img.example/src.png"}},
		"an img tag over the tag cap is ignored":          {`<img src="https://img.example/a.png" ` + strings.Repeat("x", maxTagBytes) + `>`, 8, nil},
		"an img tag under the tag cap is read":            {`<img src="https://img.example/a.png" ` + strings.Repeat("x", maxTagBytes-100) + `>`, 8, []string{"https://img.example/a.png"}},
		"an over-long inline URL takes no candidate slot": {"![a](https://img.example/" + strings.Repeat("a", maxImageURLBytes) + ".png) ![b](https://img.example/b.png)", 1, []string{"https://img.example/b.png"}},
		"an over-long img src takes no candidate slot":    {`<img src="https://img.example/` + strings.Repeat("a", maxImageURLBytes) + `"> <img src="https://img.example/b.png">`, 1, []string{"https://img.example/b.png"}},
		"two spellings of one URL are kept once":          {`![a](https://IMG.example/a.png) <img src="https://img.example/a.png">`, 8, []string{"https://img.example/a.png"}},
		"a bang behind nine backslashes is escaped":       {strings.Repeat(`\`, 9) + "![a](https://img.example/a.png)", 8, nil},
		"a bang behind ten backslashes is not":            {strings.Repeat(`\`, 10) + "![a](https://img.example/a.png)", 8, []string{"https://img.example/a.png"}},
	} {
		got := extractImageURLs(tc.md, tc.limit).urls
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

// fill repeats unit up to size bytes.
func fill(unit string, size int) string {
	return strings.Repeat(unit, size/len(unit)+1)[:size]
}

// costShapes are documents that make each part of the scan work as hard as
// it can per byte. Each is imageScanWindow bytes.
func costShapes() map[string]string {
	w := imageScanWindow
	var manyUses strings.Builder
	for i := 0; i < 200; i++ {
		manyUses.WriteString("![x][huge]")
	}
	hugeDefinition := manyUses.String() + "\n[huge]: https://img.example/" + strings.Repeat("a", w)
	var distinctUses strings.Builder
	for i := 0; i < 800; i++ {
		fmt.Fprintf(&distinctUses, "![x][d%d]", i)
	}
	var distinctDefs strings.Builder
	for i := 0; distinctDefs.Len() < w; i++ {
		fmt.Fprintf(&distinctDefs, "\n[d%d]: https://img.example/%s", i%800, strings.Repeat("b", 4000))
	}
	longURL := "https://img.example/" + strings.Repeat("a", maxImageURLBytes-30)
	var uses, same strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&uses, "![x][label%d]", i%100)
		same.WriteString("![x][same]")
	}
	return map[string]string{
		"open brackets":                         fill("[", w),
		"image opens":                           fill("![", w),
		"closes":                                fill("]", w),
		"empty images":                          fill("![](", w),
		"image then paren":                      fill("![a](h", w),
		"unclosed destinations":                 fill("![a](https://img.example/"+strings.Repeat("b", 100), w),
		"long destination":                      fill("![a]("+longURL, w),
		"destination at the cap":                fill("![a]("+longURL+strings.Repeat("c", 40)+" ", w),
		"duplicate images":                      fill("![a](https://img.example/a.png)", w),
		"distinct invalid":                      distinct("![a](https://u@h/%d.png)", w),
		"references":                            fill("![a][b]", w),
		"reference labels":                      fill("![a]["+strings.Repeat("l", 999-1), w),
		"shortcut references":                   fill("![abc]", w),
		"img starts":                            fill("<img", w),
		"img unclosed":                          fill("<img src=\"https://img.example/a.png\" "+strings.Repeat("x", 200), w),
		"img lt":                                fill("<img src=x <", w),
		"img tags":                              fill(`<img src="https://img.example/a.png">`, w),
		"img tags distinct":                     distinct(`<img src="https://img.example/%d.png?&copy;">`, w),
		"escapes":                               fill(`\`, w),
		"definitions":                           uses.String() + "\n" + fill("[label1]: https://img.example/x.png\n", w-uses.Len()-1),
		"definitions unmatched":                 uses.String() + "\n" + fill("[other]: https://img.example/x.png\n", w-uses.Len()-1),
		"definitions long labels":               uses.String() + "\n" + fill("["+strings.Repeat("q", 999)+"]: https://img.example/x.png\n", w-uses.Len()-1),
		"definition label lines":                uses.String() + "\n" + fill("[label1\n", w-uses.Len()-1),
		"definitions one label":                 same.String() + "\n" + fill("[same]: https://img.example/x.png\n", w-same.Len()-1),
		"definitions one label, one pending":    "![never][nope]" + same.String() + "\n" + fill("[same]: https://img.example/x.png\n", w-same.Len()-15),
		"definitions one label, unusable":       same.String() + "\n" + fill("[same]: ftp://img.example/x.png\n", w-same.Len()-1),
		"newlines":                              fill("\n", w),
		"distinct shortcut references":          distinct("![label%d] ", w),
		"nested collapsed references":           fill(strings.Repeat("![", 32)+strings.Repeat("c", 500)+strings.Repeat("][]", 32), w),
		"nested distinct collapsed references":  distinct("![x%d"+strings.Repeat("![", 30)+"y"+strings.Repeat("][]", 31), w),
		"nested alt labels":                     fill(strings.Repeat("!["+strings.Repeat("a", 29), 32)+strings.Repeat("]", 32), w),
		"nested long alt labels":                fill(strings.Repeat("![", 32)+strings.Repeat("a", 934)+strings.Repeat("]", 32), w),
		"nested distinct alt labels":            distinct("![a%d"+strings.Repeat("![b", 31)+strings.Repeat("]", 32), w),
		"uses then one huge definition":         hugeDefinition[:w],
		"distinct labels with long definitions": (distinctUses.String() + distinctDefs.String())[:w],
		"tab-split distinct src":                distinct("<img src=\"h\tt\tt\tp\ts://img.example/%d/"+strings.Repeat("a\t", 900)+"\">", w),
		"amp-heavy distinct src":                distinct("<img src=\"https://img.example/%d?"+strings.Repeat("&amp;", 380)+"\">", w),
		"escaped bangs":                         fill(`\![a](https://img.example/a.png)`, w),
		"long image tags":                       fill("<img src=\"https://img.example/a.png\" "+strings.Repeat("x", maxTagBytes)+">", w),
		"long distinct img src":                 distinct(`<img src="https://img.example/%d/`+strings.Repeat("s", 3500)+`">`, w),
	}
}

func distinct(format string, size int) string {
	var b strings.Builder
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, format, i)
	}
	return b.String()[:size]
}

// maxStepsPerByte is the per-byte bound the scan's header derives: at most
// two reads by the main loop or an inner scan, one by the duplicate check,
// three by normalization and one by the normalized duplicate check.
const maxStepsPerByte = 7

// maxPolicyLimit is the scan limit at the largest image cap the default
// settings allow (remote_image_max_count may not exceed max_files).
const maxPolicyLimit = 4 * 200

// TestExtractImageURLsNormalizationWork: at the largest scan limit the
// settings allow, normalizing a candidate examines a fixed number of bytes
// per candidate, whatever the document, because over-long candidates are
// refused before they are decoded.
func TestExtractImageURLsNormalizationWork(t *testing.T) {
	for name, doc := range costShapes() {
		ex := extractImageURLs(doc, maxPolicyLimit)
		if limit := ex.normalized * 3 * (maxImageURLBytes + 8); ex.normSteps > limit {
			t.Errorf("%s: normalization examined %d bytes for %d candidates (limit %d)", name, ex.normSteps, ex.normalized, limit)
		}
	}
}

// TestExtractImageURLsLinear: on every cost shape the scan examines at most
// maxStepsPerByte steps per byte of the window.
func TestExtractImageURLsLinear(t *testing.T) {
	for name, doc := range costShapes() {
		ex := extractImageURLs(doc, 128)
		if testing.Verbose() {
			t.Logf("%-32s steps/byte %.2f", name, float64(ex.steps)/float64(len(doc)))
		}
		if limit := maxStepsPerByte*len(doc) + 4096; ex.steps > limit {
			t.Errorf("%s: %d steps for %d bytes (limit %d)", name, ex.steps, len(doc), limit)
		}
	}
}

// extractAlloc returns the bytes extractImageURLs allocates on doc.
func extractAlloc(doc string, limit int) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	ex := extractImageURLs(doc, limit)
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(ex)
	return after.TotalAlloc - before.TotalAlloc
}

// allocAllowance is what extraction may allocate besides the window it is
// given: a fixed amount plus a share for each URL it may keep.
func allocAllowance(limit int) uint64 {
	return 64<<10 + uint64(limit)*4*maxImageURLBytes
}

// TestExtractImageURLsAllocBound: whatever the document, extraction
// allocates no more than allocAllowance, which depends on the number of
// URLs it may keep and not on the document's size.
func TestExtractImageURLsAllocBound(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts differ under the race detector")
	}
	for name, doc := range costShapes() {
		for _, limit := range []int{4, 128, maxPolicyLimit} {
			got, allow := extractAlloc(doc, limit), allocAllowance(limit)
			if testing.Verbose() {
				t.Logf("%-32s limit %3d alloc %8d allowance %8d", name, limit, got, allow)
			}
			if got > allow {
				t.Errorf("%s (limit %d): allocated %d bytes, allowance %d", name, limit, got, allow)
			}
		}
	}
	// Many distinct kept URLs, each at the size cap.
	var b strings.Builder
	for i := 0; b.Len() < imageScanWindow; i++ {
		fmt.Fprintf(&b, "![](https://img.example/%06d/%s)\n", i, strings.Repeat("p", maxImageURLBytes-40))
	}
	if got, allow := extractAlloc(b.String(), 128), allocAllowance(128); got > allow {
		t.Errorf("long distinct URLs: allocated %d bytes, allowance %d", got, allow)
	}
}

func TestAttrValue(t *testing.T) {
	for _, tc := range []struct {
		attrs, want string
		ok          bool
	}{
		{` src="a" alt="b"`, "a", true},
		{` alt='x' SRC='a'`, "a", true},
		{` src=a alt=b`, "a", true},
		{` src = "a"`, "a", true},
		{` src`, "", false},
		{` src="a`, "", false},
		{` srcset="a"`, "", false},
		{` data-src="a" src="b"`, "b", true},
		{` alt="src=x" src="b"`, "b", true},
		{`/src="a"`, "a", true},
	} {
		got, ok := attrValue(tc.attrs, "src")
		if got != tc.want || ok != tc.ok {
			t.Errorf("attrValue(%q) = %q, %v; want %q, %v", tc.attrs, got, ok, tc.want, tc.ok)
		}
	}
}

// TestExtractImageURLsRandomCompositions builds windows from random slices
// of every cost shape, joined in random order, and checks the per-byte
// work, per-candidate normalization work and allocation ceilings on each.
// The seed is logged; set SCION_TEST_SEED to replay a failure.
func TestExtractImageURLsRandomCompositions(t *testing.T) {
	seed := time.Now().UnixNano()
	if v := os.Getenv("SCION_TEST_SEED"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatalf("SCION_TEST_SEED: %v", err)
		}
		seed = n
	}
	t.Logf("seed %d (replay with SCION_TEST_SEED=%d)", seed, seed)
	rng := rand.New(rand.NewSource(seed))
	shapes := costShapes()
	names := make([]string, 0, len(shapes))
	for name := range shapes {
		names = append(names, name)
	}
	sort.Strings(names)
	// Each round starts with half a window of one shape, in turn, so every
	// shape is exercised at size; the rest is random slices of any shape.
	for round := 0; round < len(names); round++ {
		var b strings.Builder
		lead := names[round]
		b.WriteString(shapes[lead][:imageScanWindow/2])
		used := []string{lead + " (lead)"}
		for b.Len() < imageScanWindow {
			name := names[rng.Intn(len(names))]
			doc := shapes[name]
			n := 1 + rng.Intn(256<<10)
			off := rng.Intn(len(doc))
			piece := doc[off:min(len(doc), off+n)]
			if rng.Intn(4) == 0 {
				piece = doc[:min(len(doc), n)] // a shape's own start, where its prefix lives
			}
			b.WriteString(piece)
			used = append(used, name)
		}
		doc := b.String()[:imageScanWindow]
		limit := []int{4, 128, maxPolicyLimit}[rng.Intn(3)]
		ex := extractImageURLs(doc, limit)
		if max := maxStepsPerByte*len(doc) + 4096; ex.steps > max {
			t.Errorf("round %d (limit %d, shapes %v): %d steps for %d bytes (limit %d)", round, limit, used, ex.steps, len(doc), max)
		}
		if max := ex.normalized * 3 * (maxImageURLBytes + 8); ex.normSteps > max {
			t.Errorf("round %d (limit %d, shapes %v): normalization examined %d bytes for %d candidates", round, limit, used, ex.normSteps, ex.normalized)
		}
		if !raceEnabled {
			if got, allow := extractAlloc(doc, limit), allocAllowance(limit); got > allow {
				t.Errorf("round %d (limit %d, shapes %v): allocated %d bytes, allowance %d", round, limit, used, got, allow)
			}
		}
	}
}

// TestExtractImageURLsKeptDestinationsWork: when kept inline destinations
// fill the document, each of their bytes is read six times (the
// destination scan, the duplicate check, three normalization reads, the
// normalized duplicate check) and the main loop does not read them again.
func TestExtractImageURLsKeptDestinationsWork(t *testing.T) {
	var b strings.Builder
	for i := 0; i < maxPolicyLimit; i++ {
		fmt.Fprintf(&b, "![](https://img.example/%04d/%s)", i, strings.Repeat("p", maxImageURLBytes-40))
	}
	doc := b.String()
	ex := extractImageURLs(doc, maxPolicyLimit)
	if len(ex.urls) != maxPolicyLimit {
		t.Fatalf("kept %d URLs", len(ex.urls))
	}
	if limit := 6*len(doc) + 4096; ex.steps > limit {
		t.Errorf("%d steps for %d bytes (limit %d)", ex.steps, len(doc), limit)
	}
}
