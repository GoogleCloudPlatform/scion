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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeFailureCause(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain runtime text kept", "docker run scion-ctr-1: exit status 125", "docker run scion-ctr-1: exit status 125"},
		{"kubernetes refusal kept",
			`pods "agent-pod-4555" is forbidden: User "system:serviceaccount:team-ns:scion-sa" cannot get resource "pods"`,
			`pods "agent-pod-4555" is forbidden: User "system:serviceaccount:team-ns:scion-sa" cannot get resource "pods"`},
		{"long lower-case name kept", "pod scion-agent-a-rather-long-generated-name-0001 failed", "pod scion-agent-a-rather-long-generated-name-0001 failed"},
		{"env value blanked, key kept", "run failed: FOO=bar BAZ=qux done", "run failed: FOO=[REDACTED] BAZ=[REDACTED] done"},
		{"quoted value blanked", `bad env X="a b c"`, "bad env X=[REDACTED]"},
		{"flag value blanked", "unknown flag --env=FOO=bar", "unknown flag --env=[REDACTED]"},
		{"credential-named JSON key", `spec {"DB_PASSWORD": "hunter2-value"}`, `spec {"DB_PASSWORD": [REDACTED]}`},
		{"SCION_ YAML key", "SCION_HUB_ENDPOINT: https://hub.example.com", "SCION_HUB_ENDPOINT: [REDACTED]"},
		{"URL userinfo", "pull https://deploy:pw-value@registry.example.com/img:1 failed", "pull https://[REDACTED]@registry.example.com/img:1 failed"},
		{"authorization scheme", "header Authorization: Bearer abcdefghijklmnop0123 refused", "header Authorization: Bearer [REDACTED] refused"},
		{"JWT-shaped string", "got eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl here", "got [REDACTED] here"},
		{"PEM block", "key -----BEGIN PRIVATE KEY-----\nMIIBVQIBADANBg\n-----END PRIVATE KEY----- end", "key [REDACTED] end"},
		{"unterminated PEM block", "key -----BEGIN CERTIFICATE-----\nMIIBVQIBADANBg", "key [REDACTED]"},
		{"long hex run", "container 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef exited", "container [REDACTED] exited"},
		{"single-quoted key", "spec {'password': 'hunter2-value'}", "spec {'password': [REDACTED]}"},
		{"passphrase key", "key_passphrase: hunter2-value", "key_passphrase: [REDACTED]"},
		{"lower-case colon key", "spec password: hunter2-value end", "spec password: [REDACTED] end"},
		{"hyphenated header key", "header x-api-key: abc123 sent", "header x-api-key: [REDACTED] sent"},
		{"capitalized key, no space", "Password:hunter2", "Password:[REDACTED]"},
		{"auth as a whole key word", "auth-header: abc123 and X_AUTH=xyz", "auth-header: [REDACTED] and X_AUTH=[REDACTED]"},
		{"registry refusal kept", "unauthorized: authentication required", "unauthorized: authentication required"},
		{"EnvVar JSON value", `env [{"name":"GITHUB_PAT","value":"hunter2"}]`, `env [{"name":"GITHUB_PAT","value":[REDACTED]}]`},
		{"EnvVar %+v value", "env {Name:GITHUB_PAT Value:hunter2 ValueFrom:nil}", "env {Name:GITHUB_PAT Value:[REDACTED] ValueFrom:nil}"},
		{"basic credentials after Authorization", "Authorization: Basic dXNlcjpwYXNz", "Authorization: Basic [REDACTED]"},
		{"basic encoded credentials", "sent basic dXNlcjpwYXNzd29yZA== upstream", "sent basic [REDACTED] upstream"},
		{"basic prose kept", "basic validation failed", "basic validation failed"},
		{"long base64 run", "blob QWxhZGRpbjpvcGVuIHNlc2FtZQ0123456789AbCdEf end", "blob [REDACTED] end"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeFailureCause(tt.in); got != tt.want {
				t.Errorf("sanitizeFailureCause(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeFailureCause_Truncates(t *testing.T) {
	in := strings.Repeat("output line ", 300)
	got := sanitizeFailureCause(in)
	suffix := fmt.Sprintf("...[truncated, %d bytes]", len(in))
	if !strings.HasSuffix(got, suffix) {
		t.Fatalf("got %q, want suffix %q", got[len(got)-60:], suffix)
	}
	if body := strings.TrimSuffix(got, suffix); len(body) != maxLoggedCauseBytes || !strings.HasPrefix(in, body) {
		t.Errorf("kept %d bytes, want the first %d", len(body), maxLoggedCauseBytes)
	}

	// The cut never splits a multi-byte rune.
	multi := strings.Repeat("é", maxLoggedCauseBytes)
	got = sanitizeFailureCause(multi)
	if !utf8.ValidString(got) {
		t.Errorf("truncated text is not valid UTF-8")
	}
}

// credentialShapedValues are the values credentialShapedStartError embeds;
// none may appear in any broker log record.
var credentialShapedValues = []string{
	"sk-live-0123456789abcdefABCDEF",
	"hunter2-value",
	"pw-value-123",
	"abcdefghijklmnop0123",
	"MIIBVQIBADANBgkqhkiG9w0BAQEFAASC",
	"eyJhbGciOiJIUzI1NiJ9",
	"envvar-value-4242",
}

// credentialShapedStartError is a Manager.Start failure whose backend text
// repeats credential-shaped parts of a start spec and runs past
// maxLoggedCauseBytes.
func credentialShapedStartError() error {
	backend := "docker: Error response from daemon: failed to create task: " +
		"env FOO_API_KEY=sk-live-0123456789abcdefABCDEF " +
		`spec {"DB_PASSWORD": "hunter2-value"} ` +
		"pull https://deploy:pw-value-123@registry.example.com/img " +
		"Authorization: Bearer abcdefghijklmnop0123 " +
		`env [{"name":"GITHUB_PAT","value":"envvar-value-4242"}] ` +
		"-----BEGIN PRIVATE KEY-----\nMIIBVQIBADANBgkqhkiG9w0BAQEFAASC\n-----END PRIVATE KEY----- " +
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl " +
		strings.Repeat("container output line\n", 100)
	return fmt.Errorf("failed to launch container: %w", errors.New(backend))
}

// sanitizedCauseLogSites lists the log records pinned to pass a failure's
// cause and error text through sanitizeFailureCause: the terminal create
// failure records (synchronous create and async launch). Each listed site
// needs a case in TestCreateFailureCause_SanitizedAtEveryLogSite.
var sanitizedCauseLogSites = []string{
	"Agent create failed",
	"runLaunch: agent start failed",
}

// assertSanitizedCauseRecord checks logs hold exactly one msg record, that
// its cause and error fields are sanitized and truncated, and that no
// record holds any credential-shaped value.
func assertSanitizedCauseRecord(t *testing.T, logs, msg string) {
	t.Helper()
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == msg {
			recs = append(recs, r)
		}
	}
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want exactly 1; logs: %s", len(recs), msg, logs)
	}
	for _, field := range []string{"cause", "error"} {
		got, ok := recs[0][field].(string)
		if !ok {
			t.Fatalf("%s = %#v, want a string", field, recs[0][field])
		}
		for _, want := range []string{"FOO_API_KEY=[REDACTED]", `"DB_PASSWORD": [REDACTED]`, "https://[REDACTED]@registry.example.com", "Bearer [REDACTED]", `"value":[REDACTED]`, "...[truncated, "} {
			if !strings.Contains(got, want) {
				t.Errorf("%s lacks %q: %q", field, want, got)
			}
		}
		if len(got) > maxLoggedCauseBytes+64 {
			t.Errorf("%s is %d bytes, want it bounded near %d", field, len(got), maxLoggedCauseBytes)
		}
	}
	for _, v := range credentialShapedValues {
		if strings.Contains(logs, v) {
			t.Errorf("logs contain %q", v)
		}
	}
}

// TestCreateFailureCause_SanitizedAtEveryLogSite: a create whose runtime
// start fails with credential-shaped backend text logs one record per
// failure with that text sanitized and truncated, and the response the
// client gets is the same fixed body a plain failure gets.
func TestCreateFailureCause_SanitizedAtEveryLogSite(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"Agent create failed": func(t *testing.T) {
			const name = "agent-sync-sanitize"
			status, body, _, logs := postSyncCreateFailure(t, name, credentialShapedStartError())
			_, plainBody, _, _ := postSyncCreateFailure(t, name, errors.New("docker run scion-ctr-1: exit status 125"))
			if status != 500 || body != plainBody {
				t.Errorf("response = %d %s, want the plain failure's body %s", status, body, plainBody)
			}
			assertSanitizedCauseRecord(t, logs, "Agent create failed")
		},
		"runLaunch: agent start failed": func(t *testing.T) {
			const name = "agent-async-sanitize"
			report, logs := asyncCreateFailure(t, name, credentialShapedStartError())
			if report.ErrorCode != "runtime_error" || report.Message != "Failed to create agent" {
				t.Errorf("report = %s %q, want runtime_error with the fixed text", report.ErrorCode, report.Message)
			}
			raw, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range credentialShapedValues {
				if strings.Contains(string(raw), v) {
					t.Errorf("report contains %q", v)
				}
			}
			assertSanitizedCauseRecord(t, logs, "runLaunch: agent start failed")
		},
	}
	for _, site := range sanitizedCauseLogSites {
		run, ok := cases[site]
		if !ok {
			t.Errorf("log site %q has no case", site)
			continue
		}
		t.Run(site, run)
	}
	if len(cases) != len(sanitizedCauseLogSites) {
		t.Errorf("%d cases for %d log sites", len(cases), len(sanitizedCauseLogSites))
	}
}
