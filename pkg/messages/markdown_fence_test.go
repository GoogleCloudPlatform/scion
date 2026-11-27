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

import "testing"

func TestIsEligibleShellFenceInfoString(t *testing.T) {
	cases := []struct {
		info string
		want bool
	}{
		{"bash", true},
		{"sh", true},
		{"shell", true},
		{"console", true},
		{"  BASH  ", true},
		{"Sh title=\"deploy\"", true},
		{"bash no-execute", false},
		{"sh NO-EXECUTE", false},
		{"console foo no-execute bar", false},
		{"", false},
		{"   ", false},
		{"python", false},
		{"javascript", false},
		{"bash-script", false},
		{"no-execute sh", false},
	}
	for _, tc := range cases {
		if got := IsEligibleShellFenceInfoString(tc.info); got != tc.want {
			t.Errorf("IsEligibleShellFenceInfoString(%q) = %v, want %v", tc.info, got, tc.want)
		}
	}
}

func TestHasVerbatimExecutableShellFence(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		script string
		want   bool
	}{
		{
			name:   "simple_sh_with_trailing_lf",
			body:   "Run this:\n```sh\necho hello\n```\n",
			script: "echo hello\n",
			want:   true,
		},
		{
			name:   "simple_sh_without_trailing_lf",
			body:   "Run this:\n```sh\necho hello\n```\n",
			script: "echo hello",
			want:   true,
		},
		{
			name:   "bash_multiline_with_longer_closing_fence",
			body:   "```bash\necho one\necho two\n````\n",
			script: "echo one\necho two\n",
			want:   true,
		},
		{
			name:   "tilde_fence_console",
			body:   "~~~console\ngcloud config list\n~~~\n",
			script: "gcloud config list\n",
			want:   true,
		},
		{
			name:   "four_backtick_fence_containing_three_backticks",
			body:   "````shell\necho '```'\n````\n",
			script: "echo '```'\n",
			want:   true,
		},
		{
			name:   "crlf_body_normalized_to_lf",
			body:   "Try:\r\n```sh\r\necho a\r\necho b\r\n```\r\n",
			script: "echo a\necho b\n",
			want:   true,
		},
		{
			name:   "closing_fence_with_trailing_spaces",
			body:   "```sh\necho ok\n```   \t\n",
			script: "echo ok\n",
			want:   true,
		},
		{
			name:   "edited_bytes_rejected",
			body:   "```sh\necho hello\n```\n",
			script: "echo world\n",
			want:   false,
		},
		{
			name:   "extra_trailing_space_rejected",
			body:   "```sh\necho hello\n```\n",
			script: "echo hello \n",
			want:   false,
		},
		{
			name:   "no_execute_token_rejected",
			body:   "```sh no-execute\necho hello\n```\n",
			script: "echo hello\n",
			want:   false,
		},
		{
			name:   "python_block_rejected",
			body:   "```python\necho hello\n```\n",
			script: "echo hello\n",
			want:   false,
		},
		{
			name:   "untagged_block_rejected",
			body:   "```\necho hello\n```\n",
			script: "echo hello\n",
			want:   false,
		},
		{
			name:   "indented_opening_fence_rejected",
			body:   "  ```sh\n  echo hello\n  ```\n",
			script: "echo hello\n",
			want:   false,
		},
		{
			name:   "blockquote_nested_fence_rejected",
			body:   "> ```sh\n> echo hello\n> ```\n",
			script: "echo hello\n",
			want:   false,
		},
		{
			name:   "shell_fence_nested_inside_markdown_fence_rejected",
			body:   "~~~~markdown\n```sh\necho smuggled\n```\n~~~~\n",
			script: "echo smuggled\n",
			want:   false,
		},
		{
			name:   "shell_fence_nested_inside_no_execute_fence_rejected",
			body:   "````sh no-execute\n```sh\necho smuggled\n```\n````\n",
			script: "echo smuggled\n",
			want:   false,
		},
		{
			name:   "unclosed_fence_rejected",
			body:   "```sh\necho unclosed\n",
			script: "echo unclosed\n",
			want:   false,
		},
		{
			name:   "mismatched_closing_fence_char_rejected",
			body:   "```sh\necho hi\n~~~\n",
			script: "echo hi\n",
			want:   false,
		},
		{
			name:   "shorter_closing_fence_rejected",
			body:   "````sh\necho hi\n```\n",
			script: "echo hi\n",
			want:   false,
		},
		{
			name:   "backtick_in_info_string_rejected",
			body:   "```sh`extra\necho hi\n```\n",
			script: "echo hi\n",
			want:   false,
		},
		{
			name:   "cr_in_script_rejected",
			body:   "```sh\necho hi\n```\n",
			script: "echo hi\r\n",
			want:   false,
		},
		{
			name:   "second_block_matches_when_first_is_no_execute",
			body:   "```sh no-execute\necho example\n```\n\n```sh\necho real\n```\n",
			script: "echo real\n",
			want:   true,
		},
		{
			name:   "o2_three_space_indented_closing_fence_closes_block",
			body:   "```sh\necho hi\n   ```\nrm -rf ~\n```\n",
			script: "echo hi\n",
			want:   true,
		},
		{
			name:   "o2_three_space_indented_closing_fence_rejects_superset",
			body:   "```sh\necho hi\n   ```\nrm -rf ~\n```\n",
			script: "echo hi\n   ```\nrm -rf ~\n",
			want:   false,
		},
		{
			name:   "o2_four_space_indented_line_does_not_close_fence",
			body:   "```sh\necho hi\n    ```\n```\n",
			script: "echo hi\n    ```\n",
			want:   true,
		},
		{
			name:   "o4_trailing_blank_line_inside_fence_matches",
			body:   "```sh\necho hi\n\n```\n",
			script: "echo hi\n\n",
			want:   true,
		},
		{
			name:   "o4_trailing_blank_line_does_not_match_without_blank_line",
			body:   "```sh\necho hi\n\n```\n",
			script: "echo hi\n",
			want:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasVerbatimExecutableShellFence(tc.body, tc.script); got != tc.want {
				t.Errorf("HasVerbatimExecutableShellFence = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMarkdownCommandRequestKey(t *testing.T) {
	got := MarkdownCommandRequestKey("11111111-2222-3333-4444-555555555555", "ABCDEF0123456789fedcba9876543210")
	want := "md-11111111-2222-3333-4444-555555555555-abcdef0123456789"
	if got != want {
		t.Fatalf("MarkdownCommandRequestKey = %q, want %q", got, want)
	}
	if !ValidCommandRequestKey(got) {
		t.Fatalf("key %q failed ValidCommandRequestKey", got)
	}
}
