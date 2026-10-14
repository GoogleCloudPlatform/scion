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
		{"hub storage_error", agent.SkillResolutionError{URI: uri, Code: "storage_error", Message: "x"},
			"r", `required skill "gh://o/r/s@v" could not be resolved: resolver error (ref r)`},
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
		"https://user:pw@skills.example.test/a/b.tar.gz?sig=1&x=2#frag": "https://skills.example.test/a/b.tar.gz",
		"HTTP://skills.example.test/a?token=t":                          "http://skills.example.test/a",
		"https://skills.example.test/%zz":                               unparseableSkillRef,
		"my-registry-skill":                                             "my-registry-skill",
	}
	for in, want := range tests {
		if got := publicSkillRef(in); got != want {
			t.Errorf("publicSkillRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactURLsInText(t *testing.T) {
	in := `GCP API error (500): {"see":"https://u:pw-1@api.example.test/v1/x?sig=qv-1&token=qv-2&qv-3#qv-4"} and http://plain.example.test/p ok`
	got := redactURLsInText(in)
	for _, v := range []string{"pw-1", "qv-1", "qv-2", "qv-3", "qv-4"} {
		if strings.Contains(got, v) {
			t.Errorf("redacted text %q still carries %q", got, v)
		}
	}
	for _, kept := range []string{"GCP API error (500)", "api.example.test/v1/x", "sig=REDACTED", "token=REDACTED&REDACTED#REDACTED", "http://plain.example.test/p ok"} {
		if !strings.Contains(got, kept) {
			t.Errorf("redacted text %q lost %q", got, kept)
		}
	}
}

func TestRedactURLsInText_QueryAndFragmentForms(t *testing.T) {
	tests := map[string]string{
		"see https://h.example.test/p?qv-1":            "see https://h.example.test/p?REDACTED",
		"see https://h.example.test/p#k=qv-1":          "see https://h.example.test/p#REDACTED",
		"see https://h.example.test/p?a=qv-1&&b":       "see https://h.example.test/p?a=REDACTED&&REDACTED",
		"see https://h.example.test/p?a=qv-1, then go": "see https://h.example.test/p?a=REDACTED, then go",
		"(at https://h.example.test/p?a=qv-1).":        "(at https://h.example.test/p?a=REDACTED).",
		"no url here":                                  "no url here",
	}
	for in, want := range tests {
		if got := redactURLsInText(in); got != want {
			t.Errorf("redactURLsInText(%q) = %q, want %q", in, got, want)
		}
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
// failure text for an http(s) reference: the reference is shown without its
// userinfo, query or fragment, and the reason is kept.
func TestSkillResolutionPublicText_InstallHTTPRefRedacted(t *testing.T) {
	e := &agent.SkillResolutionError{
		URI:  "https://user:pw-1@skills.example.test/s.tar.gz?sig=qv-1#qv-2",
		Code: agent.SkillErrCodeInstallFailed, Message: "download failed with status 404",
	}
	want := `required skill "https://skills.example.test/s.tar.gz" could not be installed: download failed with status 404`
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
	want := `Failed to provision agent: required skill "https://skills.example.test/s.tar.gz" could not be resolved: skill source unavailable (status 503) (ref run-7)`
	if resp.Error.Message != want {
		t.Errorf("message = %q, want %q", resp.Error.Message, want)
	}
	if resp.Error.Details["skill"] != "https://skills.example.test/s.tar.gz" {
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
	code, _, msg := classifyStartError(context.Background(), fmt.Errorf("start: %w", e), "", "")
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
