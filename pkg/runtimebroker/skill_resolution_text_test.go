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

package runtimebroker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

func TestSkillResolutionPublicText(t *testing.T) {
	const uri = "gh://o/r/s@v"
	tests := []struct {
		name string
		err  agent.SkillResolutionError
		ref  string
		want string
	}{
		{"resolver error with status and ref",
			agent.SkillResolutionError{URI: uri, Code: agent.SkillErrCodeResolveFailed, Status: 422, Message: resolverTestBody},
			"run-1", `required skill "gh://o/r/s@v" could not be resolved: resolver error (status 422) (ref run-1)`},
		{"no status, no ref",
			agent.SkillResolutionError{URI: uri, Code: agent.SkillErrCodeResolveFailed, Message: resolverTestBody},
			"", `required skill "gh://o/r/s@v" could not be resolved: resolver error`},
		{"not found", agent.SkillResolutionError{URI: uri, Code: agent.SkillErrCodeNotFound, Status: 404, Message: "x"},
			"r", `required skill "gh://o/r/s@v" could not be resolved: skill not found (status 404) (ref r)`},
		{"forbidden", agent.SkillResolutionError{URI: uri, Code: agent.SkillErrCodeForbidden, Message: "x"},
			"r", `required skill "gh://o/r/s@v" could not be resolved: access denied (ref r)`},
		{"rate limited", agent.SkillResolutionError{URI: uri, Code: agent.SkillErrCodeRateLimited, Status: 429, Message: "x"},
			"r", `required skill "gh://o/r/s@v" could not be resolved: rate limited by the skill source (status 429) (ref r)`},
		{"timeout", agent.SkillResolutionError{URI: uri, Code: agent.SkillErrCodeTimeout, Message: "x"},
			"r", `required skill "gh://o/r/s@v" could not be resolved: timed out reaching the skill source (ref r)`},
		{"upstream unavailable", agent.SkillResolutionError{URI: uri, Code: agent.SkillErrCodeUpstreamUnavailable, Status: 503, Message: "x"},
			"r", `required skill "gh://o/r/s@v" could not be resolved: skill source unavailable (status 503) (ref r)`},
		{"unreachable", agent.SkillResolutionError{URI: uri, Code: agent.SkillErrCodeUnreachable, Message: "x"},
			"r", `required skill "gh://o/r/s@v" could not be resolved: skill source unreachable (ref r)`},
		{"invalid uri", agent.SkillResolutionError{URI: uri, Code: agent.SkillErrCodeInvalidURI, Message: "x"},
			"r", `required skill "gh://o/r/s@v" could not be resolved: invalid skill URI (ref r)`},
		{"hub federation_error with status", agent.SkillResolutionError{URI: uri, Code: "federation_error", Status: 502, Message: "x"},
			"r", `required skill "gh://o/r/s@v" could not be resolved: skill registry request failed (status 502) (ref r)`},
		{"hub storage_error", agent.SkillResolutionError{URI: uri, Code: "storage_error", Message: "x"},
			"r", `required skill "gh://o/r/s@v" could not be resolved: skill files unavailable (ref r)`},
		{"install failure keeps its public text",
			agent.SkillResolutionError{URI: uri, Code: agent.SkillErrCodeInstallFailed, Message: "download failed with status 404"},
			"r", `required skill "gh://o/r/s@v" could not be installed: download failed with status 404`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := skillResolutionPublicText(&tt.err, tt.ref); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSkillResolutionPublicText_InstallUnchangedFromError(t *testing.T) {
	e := &agent.SkillResolutionError{URI: "gh://o/r/s@v", Code: agent.SkillErrCodeInstallFailed, Message: "install failed on the broker"}
	if got := skillResolutionPublicText(e, "r"); got != e.Error() {
		t.Errorf("install text = %q, want the existing text %q", got, e.Error())
	}
}

func TestPublicSkillRef(t *testing.T) {
	tests := map[string]string{
		"gh://o/r/s@v": "gh://o/r/s@v",
		"https://user:pw@skills.example.test/a/b.tar.gz?sig=1&x=2#frag": "https://(redacted URL)",
		"https://skills.example.test/a/b.tar.gz?v=1":                    "https://(redacted URL)",
		"https://skills.example.test/a/b.tar.gz#frag":                   "https://(redacted URL)",
		"https://alice:p w-1@skills.example.test/x":                     "https://(redacted URL)",
		"https://skills.example.test/a b.tar.gz":                        "https://(redacted URL)",
		"HTTP://skills.example.test/a?token=t":                          "http://(redacted URL)",
		"HTTP://skills.example.test/a":                                  "http://skills.example.test/a",
		"https:skills.example.test/a":                                   "https://skills.example.test/a",
		"https://skills.example.test/%zz":                               "https://(unparseable URL)",
		"my-registry-skill":                                             "my-registry-skill",
		"my-skill@1.0":                                                  "my-skill@1.0",
		"git+https://u:pw-1@h.example.test/x?k=qv-2":                    "(redacted URL)",
		" https://u:pw-1@h.example.test/x":                              "(redacted URL)",
		"\thttps://h.example.test/x":                                    "(redacted URL)",
		"ssh://u:pw-1@h.example.test/x":                                 "(redacted URL)",
		"s3://bucket/o?k=qv-1":                                          "(redacted URL)",
		"gh://owner/repo/skill@main":                                    "gh://owner/repo/skill@main",
		"GH://owner/repo/skill@main":                                    "GH://owner/repo/skill@main",
		"gh://u:pw-1@owner/repo/skill":                                  "(redacted URL)",
		"gh://owner/repo/skill@main?token=NAME":                         "(redacted URL)",
		"gh://owner/repo/https://h.example.test/x":                      "(redacted URL)",
		"skill://scion/core/scion@^1.0":                                 "skill://scion/core/scion@^1.0",
		"skill://scion/core/scion?k=qv-1":                               "(redacted URL)",
		"skill://scion/core/sc ion":                                     "(redacted URL)",
		"gcp-skill://reg/some-skill":                                    "gcp-skill://reg/some-skill",
		"gcp-skill://reg/some-skill#qv-1":                               "(redacted URL)",
	}
	for in, want := range tests {
		if got := publicSkillRef(in); got != want {
			t.Errorf("publicSkillRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSkillLogText_RefWithRawSpaceInPassword checks the skill reference is
// decided as a whole in the log text and in the user-visible text, so a raw
// space in its password does not leave the rest of it as plain text.
func TestSkillLogText_RefWithRawSpaceInPassword(t *testing.T) {
	e := &agent.SkillResolutionError{
		URI: "https://alice:p w-1@skills.example.test/x", Code: agent.SkillErrCodeResolveFailed,
		Message: "boom at https://h.example.test/x?k=qv-1",
	}
	if got, want := skillLogText(e), `required skill "https://(redacted URL)" could not be resolved: (resolver message withheld)`; got != want {
		t.Errorf("skillLogText = %q, want %q", got, want)
	}
	if got, want := skillResolutionPublicText(e, "r"), `required skill "https://(redacted URL)" could not be resolved: resolver error (ref r)`; got != want {
		t.Errorf("public text = %q, want %q", got, want)
	}
	if got := spanErrorText(fmt.Errorf("provision: %w", e)); strings.Contains(got, "w-1") || strings.Contains(got, "qv-1") {
		t.Errorf("span text %q carries the password or the query value", got)
	}
}

func TestRedactURLsInText(t *testing.T) {
	in := `GCP API error (500): {"see":"https://u:pw-1@api.example.test/v1/x?sig=qv-1&token=qv-2&qv-3#qv-4"} and http://plain.example.test/p ok`
	want := `GCP API error (500): {"see":"https://(redacted URL) and http://plain.example.test/p ok`
	if got := agent.RedactURLsInText(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRedactURLsInText_Forms has one case per URL form the redaction must
// handle.
func TestRedactURLsInText_Forms(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"query part with no =",
			"see https://h.example.test/p?qv-1 now", "see https://(redacted URL) now"},
		{"fragment only",
			"see https://h.example.test/p#k=qv-1 now", "see https://(redacted URL) now"},
		{"query and fragment",
			"see https://h.example.test/p?a=qv-1#qv-2", "see https://(redacted URL)"},
		{"empty query parts",
			"see https://h.example.test/p?a=qv-1&&b", "see https://(redacted URL)"},
		{"userinfo",
			"see https://u:pw-1@h.example.test/p.", "see https://(redacted URL)"},
		{"two URLs joined with a comma",
			"https://a.example.test/p,https://u:pw-1@b.example.test/x?qv-1", "https://(redacted URL)"},
		{"two URLs joined with no separator",
			"https://a.example.test/p?qv-1https://u:pw-1@b.example.test/x", "https://(redacted URL)"},
		{"two URLs joined with no query or @",
			"https://a.example.test/p,https://b.example.test/x", "https://(redacted URL)"},
		{"nested URL in a query, with userinfo",
			"see https://u:pw-1@h.example.test/x?next=https://o.example.test/y&k=qv-2 now", "see https://(redacted URL) now"},
		{"nested URL in a query",
			"see https://h.example.test/x?next=https://o.example.test/y&k=qv-2 now", "see https://(redacted URL) now"},
		{"nested URL in a fragment",
			"see https://h.example.test/x#https://o.example.test/y&k=qv-2 now", "see https://(redacted URL) now"},
		{"nested scheme before an @",
			"see https://u:12/x-https://@h.example.test/ now", "see https://(redacted URL) now"},
		{"raw quote inside the query",
			`see https://h.example.test/p?a="qv-1"&b=qv-2 now`, "see https://(redacted URL) now"},
		{"raw angle brackets inside the query",
			"see https://h.example.test/p?a=<qv-1>&b=qv-2 now", "see https://(redacted URL) now"},
		{"raw quote inside the userinfo",
			`see https://u:p"w-1@h.example.test/p now`, "see https://(redacted URL) now"},
		{"space inside the query ends the URL",
			"see https://h.example.test/p?a=qv-1 b=c", "see https://(redacted URL) b=c"},
		{"raw space in the password ends the URL (free-text limit)",
			"see https://alice:p w-1@h.example.test/x now", "see https://(unparseable URL) w-1@h.example.test/x now"},
		{"trailing punctuation in a query value",
			"see https://h.example.test/p?a=qv-1).", "see https://(redacted URL)"},
		{"trailing punctuation in a fragment",
			"see https://h.example.test/p#qv-1,", "see https://(redacted URL)"},
		{"trailing punctuation after a path",
			`(at "https://h.example.test/p").`, `(at "https://h.example.test/p").`},
		{"trailing punctuation after an @",
			"(see https://u:pw-1@h.example.test/p).", "(see https://(redacted URL)"},
		{"IPv6 host",
			"see https://[::1] now", "see https://[::1] now"},
		{"IPv6 host with port in parentheses",
			"(see https://[2001:db8::1]:8443/p)", "(see https://[2001:db8::1]:8443/p)"},
		{"bracket closing prose",
			"[see https://h.example.test/p]", "[see https://h.example.test/p]"},
		{"upper-case scheme",
			"HTTPS://h.example.test/p now", "https://h.example.test/p now"},
		{"upper-case scheme with a query",
			"HTTPS://h.example.test/p?qv-1", "https://(redacted URL)"},
		{"no // after the scheme",
			"see https:u:pw-1@h.example.test/x now", "see https://(redacted URL) now"},
		{"one / after the scheme",
			"see https:/u:pw-1@h.example.test/x now", "see https://(redacted URL) now"},
		{"no // and nothing to redact",
			"see http:h.example.test/p now", "see http://h.example.test/p now"},
		{"backslashes after the scheme",
			`see https:\\u:pw-1@h.example.test/x now`, "see https://(redacted URL) now"},
		{"scheme with nothing after it",
			"net/http: request canceled; see https:// now", "net/http: request canceled; see https:// now"},
		{"artifact path",
			"https://h.example.test/api/v1/artifacts/tok-1/x", "https://h.example.test" + logging.RedactedArtifactPath},
		{"# in the userinfo",
			"see https://u:p#w-1@h.example.test/x now", "see https://(redacted URL) now"},
		{"? in the userinfo",
			"see https://u:p?w-1@h.example.test/x now", "see https://(redacted URL) now"},
		{"/ in the userinfo",
			"see https://u:pw/-1@h.example.test/x now", "see https://(redacted URL) now"},
		{"numeric password with / in the userinfo",
			"see https://u:12/34@h.example.test now", "see https://(redacted URL) now"},
		{"@ in a query value only",
			"see https://h.example.test/x?e=qv-1@b.example.test now", "see https://(redacted URL) now"},
		{"@ in a fragment only",
			"see https://h.example.test/x#qv-1@b now", "see https://(redacted URL) now"},
		{"@ in a path",
			"see https://h.example.test/o/r/s@main now", "see https://(redacted URL) now"},
		{"userinfo and an @ in a query value",
			"see https://u:pw-1@h.example.test/x?e=qv-1@b.example.test&k=qv-2 now", "see https://(redacted URL) now"},
		{"@ in a path and in a query value",
			"see https://h.example.test/s@main?e=qv-1@b/qv-2 now", "see https://(redacted URL) now"},
		{"single-label host with an @ in a query value",
			"see http://hub/x?e=qv-1@b&k=qv-2 now", "see http://(redacted URL) now"},
		{"single-label host with port",
			"see http://hub:8080/x now", "see http://hub:8080/x now"},
		{"host that does not parse",
			"see https://h%zz/api/v1/artifacts/tok-1 now", "see https://(unparseable URL) now"},
		{"other schemes are out of scope",
			"gs://bucket/o?qv-1 ssh://u@h", "gs://bucket/o?qv-1 ssh://u@h"},
		{"no url", "no url here?", "no url here?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agent.RedactURLsInText(tt.in); got != tt.want {
				t.Errorf("agent.RedactURLsInText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSkillResolutionPublicText_LongControlByteRefKeepsCorrelationID checks
// the cap applies after quoting: each control byte quotes as four bytes.
func TestSkillResolutionPublicText_LongControlByteRefKeepsCorrelationID(t *testing.T) {
	const runID = "0f8e2c4a-1b3d-4e5f-8a9b-0c1d2e3f4a5b"
	e := &agent.SkillResolutionError{URI: strings.Repeat("\x01", 300), Code: agent.SkillErrCodeInvalidURI}
	got := skillResolutionPublicText(e, runID)
	if len(got) > 512 {
		t.Errorf("text is %d bytes, want at most 512: %q", len(got), got)
	}
	if !strings.HasSuffix(got, "invalid skill URI (ref "+runID+")") {
		t.Errorf("text %q does not end with the phrase and correlation id", got)
	}
	q := quotedSkillRef(e.URI)
	if !strings.HasPrefix(q, `"\x01`) || !strings.HasSuffix(q, `\x01…"`) || len(q) > maxPublicSkillRefLen+2+len("…") {
		t.Errorf("quotedSkillRef = %q (%d bytes), want whole escapes, an ellipsis and at most %d bytes", q, len(q), maxPublicSkillRefLen+2+len("…"))
	}
	if short := quotedSkillRef("gh://o/r/s@v"); short != `"gh://o/r/s@v"` {
		t.Errorf("quotedSkillRef(short) = %q, want it quoted as by %%q", short)
	}
}

// TestSkillResolutionPublicText_LongRefKeepsCorrelationID checks the shown
// reference is capped, so the correlation id survives the hub's cut of a
// stored failure reason to 512 bytes.
func TestSkillResolutionPublicText_LongRefKeepsCorrelationID(t *testing.T) {
	const runID = "0f8e2c4a-1b3d-4e5f-8a9b-0c1d2e3f4a5b"
	long := "https://skills.example.test/" + strings.Repeat("a", 600) + ".tar.gz"
	e := &agent.SkillResolutionError{URI: long, Code: agent.SkillErrCodeUpstreamUnavailable, Status: 503}
	got := skillResolutionPublicText(e, runID)
	if len(got) > 512 {
		t.Errorf("text is %d bytes, want at most 512: %q", len(got), got)
	}
	if !strings.HasSuffix(got, "(status 503) (ref "+runID+")") {
		t.Errorf("text %q does not end with the status and correlation id", got)
	}
	shown := publicSkillRef(long)
	if !strings.HasSuffix(shown, "…") || len(shown) != maxPublicSkillRefLen+len("…") {
		t.Errorf("publicSkillRef = %q (%d bytes), want %d bytes plus an ellipsis", shown, len(shown), maxPublicSkillRefLen)
	}
	if got := capText("ab"+strings.Repeat("é", 200), 5); got != "abé…" {
		t.Errorf("capText cut inside a rune: %q", got)
	}
}

// TestSkillResolutionPublicText_InstallHTTPRefRedacted pins the install
// failure text for an http(s) reference with userinfo, a query and a
// fragment: the reference is shown as a redacted URL, and the reason is
// kept.
func TestSkillResolutionPublicText_InstallHTTPRefRedacted(t *testing.T) {
	e := &agent.SkillResolutionError{
		URI:  "https://user:pw-1@skills.example.test/s.tar.gz?sig=qv-1#qv-2",
		Code: agent.SkillErrCodeInstallFailed, Message: "download failed with status 404",
	}
	want := `required skill "https://(redacted URL)" could not be installed: download failed with status 404`
	if got := skillResolutionPublicText(e, "r"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSkillResolutionCorrelationID(t *testing.T) {
	if got := skillResolutionCorrelationID("run-9"); got != "run-9" {
		t.Errorf("got %q, want the run id", got)
	}
	a, b := skillResolutionCorrelationID(""), skillResolutionCorrelationID("")
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(a) || a == b {
		t.Errorf("generated ids %q, %q: want two distinct 16-hex-digit ids", a, b)
	}
}

func TestSkillResolutionFailed_FixedReasonPhrase(t *testing.T) {
	w := httptest.NewRecorder()
	SkillResolutionFailed(w, &agent.SkillResolutionError{
		URI: "https://skills.example.test/s.tar.gz?sig=qv-1", Code: agent.SkillErrCodeUpstreamUnavailable,
		Status: 503, Message: resolverTestBody,
	}, "run-7")
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := `Failed to provision agent: required skill "https://(redacted URL)" could not be resolved: skill source unavailable (status 503) (ref run-7)`
	if resp.Error.Message != want {
		t.Errorf("message = %q, want %q", resp.Error.Message, want)
	}
	if resp.Error.Details["skill"] != "https://(redacted URL)" {
		t.Errorf("details.skill = %v, want the redacted reference", resp.Error.Details["skill"])
	}
	if strings.Contains(w.Body.String(), "ARBITRARY-UPSTREAM-BODY") || strings.Contains(w.Body.String(), "qv-1") {
		t.Errorf("body carries resolver text or the query: %s", w.Body.String())
	}
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
}

func TestClassifyStartError_SkillResolverError_FixedReasonPhrase(t *testing.T) {
	e := &agent.SkillResolutionError{URI: "gh://o/r/s@v", Code: agent.SkillErrCodeResolveFailed, Status: 422, Message: resolverTestBody}
	code, _, msg := classifyStartError(context.Background(), fmt.Errorf("start: %w", e), "", "", store.LaunchKindCreate)
	if code != ErrCodeSkillResolution {
		t.Fatalf("code = %q, want %q", code, ErrCodeSkillResolution)
	}
	if want := `required skill "gh://o/r/s@v" could not be resolved: resolver error (status 422)`; msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
}

func TestAsyncLaunch_SkillResolverError_StatusInStoredText(t *testing.T) {
	e := resolverTestError()
	e.Status = http.StatusUnprocessableEntity
	report, logs := asyncCreateFailure(t, "skill-resolver-async-status", e)
	want := `required skill "` + resolverTestSkill + `" could not be resolved: resolver error (status 422) (ref ` + startErrTextRunID + `)`
	if report.Message != want {
		t.Errorf("stored message = %q, want %q", report.Message, want)
	}
	rec := logRecord(t, logs, resolverLogMsg)
	if rec["status"] != float64(http.StatusUnprocessableEntity) {
		t.Errorf("log status = %v, want 422", rec["status"])
	}
	assertResolverLogRecord(t, logs, "runLaunch: agent start failed", startErrTextRunID)
}

// TestSkillLogText_SchemeAfterOtherText checks a reference with text before
// its scheme is redacted whole, and its resolver message withheld, in the
// log text and the user-visible text.
func TestSkillLogText_SchemeAfterOtherText(t *testing.T) {
	const uri = "git+https://u:pw-1@h.example.test/x?k=qv-2"
	e := &agent.SkillResolutionError{
		URI: uri, Code: "unsupported_scheme",
		Message: `no resolver registered for scheme "git+https"; cannot fetch ` + uri,
	}
	want := `required skill "(redacted URL)" could not be resolved: (resolver message withheld)`
	if got := skillLogText(e); got != want {
		t.Errorf("skillLogText = %q, want %q", got, want)
	}
	if got, want := skillResolutionPublicText(e, "r"), `required skill "(redacted URL)" could not be resolved: resolver error (ref r)`; got != want {
		t.Errorf("public text = %q, want %q", got, want)
	}
}

// TestSkillLogText_BareSchemeRefLeavesMessageAlone checks a bare-scheme
// reference does not rewrite the scheme text in the message: the message is
// withheld whole.
func TestSkillLogText_BareSchemeRefLeavesMessageAlone(t *testing.T) {
	e := &agent.SkillResolutionError{URI: "http:", Code: agent.SkillErrCodeResolveFailed, Message: "net/http: request canceled"}
	if got, want := skillLogText(e), `required skill "http://" could not be resolved: (resolver message withheld)`; got != want {
		t.Errorf("skillLogText = %q, want %q", got, want)
	}
}

func TestSpanErrorText_ContextSuffix(t *testing.T) {
	e := &agent.SkillResolutionError{URI: "gh://o/r/s@v", Code: agent.SkillErrCodeTimeout, Message: "timed out"}
	base := `required skill "gh://o/r/s@v" could not be resolved: timed out`
	tests := map[string]error{
		base + "; context deadline exceeded": fmt.Errorf("provision: %w: %w", e, context.DeadlineExceeded),
		base + "; context canceled":          fmt.Errorf("provision: %w: %w", e, context.Canceled),
		base:                                 fmt.Errorf("provision: %w", e),
	}
	for want, err := range tests {
		if got := spanErrorText(err); got != want {
			t.Errorf("spanErrorText(%v) = %q, want %q", err, got, want)
		}
	}
}
