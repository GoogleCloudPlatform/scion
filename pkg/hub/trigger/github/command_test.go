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

package github

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCommand_Grammar(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		slug    string
		aliases []string
		want    CommandMatch
	}{
		{
			name: "simple leading command",
			body: "@scion-app /review",
			slug: "scion-app",
			want: CommandMatch{
				Mentioned:   true,
				HasCommand:  true,
				Command:     "review",
				CommandArgs: "",
				CommandLine: "@scion-app /review",
			},
		},
		{
			name: "text before mention on same line ignored",
			body: "Thanks! @scion-app /review please check auth",
			slug: "scion-app",
			want: CommandMatch{
				Mentioned:   true,
				HasCommand:  true,
				Command:     "review",
				CommandArgs: "please check auth",
				CommandLine: "Thanks! @scion-app /review please check auth",
			},
		},
		{
			name: "case insensitive mention and command lowercased",
			body: "Hey @SCION-APP   /ReViEw_Fast-2   --FixAll  ",
			slug: "scion-app",
			want: CommandMatch{
				Mentioned:   true,
				HasCommand:  true,
				Command:     "review_fast-2",
				CommandArgs: "--FixAll",
				CommandLine: "Hey @SCION-APP   /ReViEw_Fast-2   --FixAll",
			},
		},
		{
			name:    "configured alias matches case insensitively",
			body:    "@Scion /implement phase 2",
			slug:    "my-org-github-app",
			aliases: []string{"@scion", "helper-bot"},
			want: CommandMatch{
				Mentioned:   true,
				HasCommand:  true,
				Command:     "implement",
				CommandArgs: "phase 2",
				CommandLine: "@Scion /implement phase 2",
			},
		},
		{
			name:    "alias prefix collision prefers full token boundary",
			body:    "@scion-other /review\n@scion /plan v1",
			slug:    "scion",
			aliases: []string{"scion-helper"},
			want: CommandMatch{
				Mentioned:   true,
				HasCommand:  true,
				Command:     "plan",
				CommandArgs: "v1",
				CommandLine: "@scion /plan v1",
			},
		},
		{
			name: "first command in reading order wins across lines and same line",
			body: "Intro paragraph.\r\n@scion /validate unit\r\n@scion /fix lint",
			slug: "scion",
			want: CommandMatch{
				Mentioned:   true,
				HasCommand:  true,
				Command:     "validate",
				CommandArgs: "unit",
				CommandLine: "@scion /validate unit",
			},
		},
		{
			name: "first command on same line wins when two mentions appear",
			body: "@scion /ping first and @scion /pong second",
			slug: "scion",
			want: CommandMatch{
				Mentioned:   true,
				HasCommand:  true,
				Command:     "ping",
				CommandArgs: "first and @scion /pong second",
				CommandLine: "@scion /ping first and @scion /pong second",
			},
		},
		{
			name: "bare mention on earlier line then command on later line",
			body: "cc @scion for visibility\nnow @scion /fix tests",
			slug: "scion",
			want: CommandMatch{
				Mentioned:   true,
				HasCommand:  true,
				Command:     "fix",
				CommandArgs: "tests",
				CommandLine: "now @scion /fix tests",
			},
		},
		{
			name: "mention without command sets Mentioned only",
			body: "Hey (@scion), could you take a look? Also **@scion**!",
			slug: "scion",
			want: CommandMatch{
				Mentioned:  true,
				HasCommand: false,
			},
		},
		{
			name: "email and intraword at-sign are not mentions",
			body: "Contact dev@scion.io or foo@scion /review or bar_@scion /review",
			slug: "scion",
			want: CommandMatch{
				Mentioned:  false,
				HasCommand: false,
			},
		},
		{
			name: "command name bounds 32 chars max and starts with ascii letter",
			body: "@scion /a23456789012345678901234567890123 too-long\n@scion /9bad\n@scion /usr/bin/env\n@scion /a2345678901234567890123456789012 ok",
			slug: "scion",
			want: CommandMatch{
				Mentioned:   true,
				HasCommand:  true,
				Command:     "a2345678901234567890123456789012",
				CommandArgs: "ok",
				CommandLine: "@scion /a2345678901234567890123456789012 ok",
			},
		},
		{
			name: "fenced code blocks backtick and tilde and language-tagged ignored",
			body: "Example:\r\n```sh\r\n@scion /review\r\n```\r\n~~~\r\n@scion /fix\r\n~~~\r\nDone.",
			slug: "scion",
			want: CommandMatch{
				Mentioned:  false,
				HasCommand: false,
			},
		},
		{
			name: "unclosed fence ignores through EOF",
			body: "Log:\n```\n@scion /review",
			slug: "scion",
			want: CommandMatch{
				Mentioned:  false,
				HasCommand: false,
			},
		},
		{
			name: "nested shorter fence or info-tagged fence inside block does not close it",
			body: "````markdown\n```go\n@scion /review\n```\n````\n@scion /plan real",
			slug: "scion",
			want: CommandMatch{
				Mentioned:   true,
				HasCommand:  true,
				Command:     "plan",
				CommandArgs: "real",
				CommandLine: "@scion /plan real",
			},
		},
		{
			name: "blockquote lines ignored including indented quote",
			body: "> @scion /review\n  > @scion /fix\nPlease wait.",
			slug: "scion",
			want: CommandMatch{
				Mentioned:  false,
				HasCommand: false,
			},
		},
		{
			name: "inline code spans ignored including multi-backtick spans",
			body: "Do not run `@scion /review` or `` `@scion /fix` `` yet.",
			slug: "scion",
			want: CommandMatch{
				Mentioned:  false,
				HasCommand: false,
			},
		},
		{
			name: "inline code in args is preserved verbatim",
			body: "See `@scion /ignored` example; @scion /fix `pkg/hub/server.go` now",
			slug: "scion",
			want: CommandMatch{
				Mentioned:   true,
				HasCommand:  true,
				Command:     "fix",
				CommandArgs: "`pkg/hub/server.go` now",
				CommandLine: "See `@scion /ignored` example; @scion /fix `pkg/hub/server.go` now",
			},
		},
		{
			name: "mention in prose followed by inline-code command records mention only",
			body: "@scion `/review`",
			slug: "scion",
			want: CommandMatch{
				Mentioned:  true,
				HasCommand: false,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseCommand(tc.body, tc.slug, tc.aliases, MaxCommandArgsBytes, MaxCommandLineBytes)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseCommand_ArgsAndLineBoundsUTF8(t *testing.T) {
	// Build multibyte UTF-8 args ('é' is 2 bytes, '🚀' is 4 bytes) exceeding 1 KiB
	// right across a multibyte rune boundary so naive byte slicing would corrupt UTF-8.
	repeated := strings.Repeat("🚀", 300) // 1200 bytes
	body := "@scion /review " + repeated

	got := ParseCommand(body, "scion", nil, MaxCommandArgsBytes, MaxCommandLineBytes)
	require.True(t, got.Mentioned)
	require.True(t, got.HasCommand)
	assert.Equal(t, "review", got.Command)
	assert.LessOrEqual(t, len(got.CommandArgs), MaxCommandArgsBytes)
	assert.Equal(t, 1024, len(got.CommandArgs)) // 256 * 4 = 1024
	assert.True(t, utf8.ValidString(got.CommandArgs))

	// Shift by 1 ASCII byte so 1024 lands mid-rune (1 + 255*4 = 1021 <= 1024, 1 + 256*4 = 1025 > 1024).
	bodyShifted := "@scion /review x" + repeated
	gotShifted := ParseCommand(bodyShifted, "scion", nil, MaxCommandArgsBytes, MaxCommandLineBytes)
	assert.Equal(t, 1021, len(gotShifted.CommandArgs))
	assert.True(t, utf8.ValidString(gotShifted.CommandArgs))
	assert.LessOrEqual(t, len(gotShifted.CommandLine), MaxCommandLineBytes)
	assert.True(t, utf8.ValidString(gotShifted.CommandLine))
}
