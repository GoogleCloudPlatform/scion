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

package envpolicy

import (
	"slices"
	"strings"
	"testing"
)

func TestHookDenied_StaticTable(t *testing.T) {
	exactDenied := []string{
		"BASH_ENV",
		"ENV",
		"ZDOTDIR",
		"PROMPT_COMMAND",
		"SHELLOPTS",
		"BASHOPTS",
		"PS4",
		"IFS",
		"CDPATH",
		"GLOBIGNORE",
		"PYTHONPATH",
		"PYTHONHOME",
		"PYTHONSTARTUP",
		"PYTHONUSERBASE",
		"PYTHONINSPECT",
		"NODE_OPTIONS",
		"NODE_PATH",
		"PERL5LIB",
		"PERL5OPT",
		"PERLLIB",
		"RUBYLIB",
		"RUBYOPT",
		"JAVA_TOOL_OPTIONS",
		"_JAVA_OPTIONS",
		"JDK_JAVA_OPTIONS",
		"GCONV_PATH",
		"XDG_CONFIG_HOME",
		"XDG_DATA_DIRS",
		"GIT_EXEC_PATH",
		"GIT_TEMPLATE_DIR",
		"GIT_SSH",
		"GIT_SSH_COMMAND",
		"GIT_PROXY_COMMAND",
		"GIT_ASKPASS",
		"SSH_ASKPASS",
		"GIT_EXTERNAL_DIFF",
		"GIT_PAGER",
		"GIT_EDITOR",
		"PAGER",
		"EDITOR",
		"VISUAL",
		"LESSOPEN",
		"LESSCLOSE",
		"GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_CEILING_DIRECTORIES",
		"SCION_EXTRA_PATH",
	}

	prefixDenied := []string{
		"LD_PRELOAD",
		"LD_LIBRARY_PATH",
		"LD_AUDIT",
		"DYLD_INSERT_LIBRARIES",
		"DYLD_LIBRARY_PATH",
		"GIT_CONFIG",
		"GIT_CONFIG_COUNT",
		"GIT_CONFIG_KEY_0",
		"GIT_CONFIG_VALUE_0",
		"GIT_CONFIG_PARAMETERS",
		"GIT_CONFIG_GLOBAL",
		"GIT_CONFIG_SYSTEM",
		"BASH_FUNC_myfunc%%",
	}

	benign := []string{
		"HOME",
		"PATH",
		"LANG",
		"TERM",
		"TZ",
		"USER",
		"LOGNAME",
		"GITHUB_TOKEN",
		"HTTP_PROXY",
		"HTTPS_PROXY",
		"NO_PROXY",
		"SSL_CERT_FILE",
		"GIT_SSL_CAINFO",
		"GIT_TERMINAL_PROMPT",
		"OPERATOR_CUSTOM_VAR",
		"SCION_AGENT_NAME",
		"",
	}

	var zeroSet Set

	for _, name := range exactDenied {
		t.Run("exact/"+name, func(t *testing.T) {
			if !HookDenied(name) {
				t.Errorf("HookDenied(%q) = false, want true", name)
			}
			if !zeroSet.Withholds(name) {
				t.Errorf("Set{}.Withholds(%q) = false, want true", name)
			}
		})
	}

	for _, name := range prefixDenied {
		t.Run("prefix/"+name, func(t *testing.T) {
			if !HookDenied(name) {
				t.Errorf("HookDenied(%q) = false, want true", name)
			}
			if !zeroSet.Withholds(name) {
				t.Errorf("Set{}.Withholds(%q) = false, want true", name)
			}
		})
	}

	for _, name := range benign {
		label := name
		if label == "" {
			label = "empty"
		}
		t.Run("benign/"+label, func(t *testing.T) {
			if HookDenied(name) {
				t.Errorf("HookDenied(%q) = true, want false", name)
			}
			if zeroSet.Withholds(name) {
				t.Errorf("Set{}.Withholds(%q) = true, want false", name)
			}
		})
	}
}

func TestStaticDenylist_CoversAllEntriesAndIsDefensiveCopy(t *testing.T) {
	list := StaticDenylist()
	wantLen := len(staticExactDenied) + len(staticPrefixDenied)
	if len(list) != wantLen {
		t.Fatalf("len(StaticDenylist()) = %d, want %d", len(list), wantLen)
	}
	if !slices.IsSorted(list) {
		t.Errorf("StaticDenylist() is not sorted: %v", list)
	}
	for _, entry := range list {
		probe := strings.TrimSuffix(entry, "*")
		if strings.HasSuffix(entry, "*") {
			probe += "TEST"
		}
		if !HookDenied(probe) {
			t.Errorf("StaticDenylist entry %q (probe %q) not denied by HookDenied", entry, probe)
		}
	}
	list[0] = "MUTATED"
	if StaticDenylist()[0] == "MUTATED" {
		t.Error("mutating StaticDenylist() return value affected subsequent calls")
	}
}

func TestSet_WithholdsDynamicAndStatic(t *testing.T) {
	s := NewSet("CUSTOM_FOO", "PATH", " ", "")
	if !s.Withholds("CUSTOM_FOO") {
		t.Error("expected CUSTOM_FOO to be withheld")
	}
	if !s.Withholds("PATH") {
		t.Error("expected PATH to be withheld")
	}
	if !s.Withholds("LD_PRELOAD") {
		t.Error("expected static LD_PRELOAD to be withheld")
	}
	if s.Withholds("OPERATOR_KEY") {
		t.Error("expected OPERATOR_KEY not to be withheld")
	}
	wantExtra := []string{"CUSTOM_FOO", "PATH"}
	if got := s.Extra(); !slices.Equal(got, wantExtra) {
		t.Errorf("Extra() = %v, want %v", got, wantExtra)
	}
}

func TestWithheldMarker_FormatAndParse(t *testing.T) {
	cases := []struct {
		name       string
		input      []string
		wantFormat string
		wantParsed []string
	}{
		{
			name:       "empty",
			input:      nil,
			wantFormat: "",
			wantParsed: nil,
		},
		{
			name:       "whitespace only",
			input:      []string{"", "  "},
			wantFormat: "",
			wantParsed: nil,
		},
		{
			name:       "sorts and deduplicates",
			input:      []string{"Z_KEY", "A_KEY", "M_KEY", "A_KEY", ""},
			wantFormat: "A_KEY,M_KEY,Z_KEY",
			wantParsed: []string{"A_KEY", "M_KEY", "Z_KEY"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotFmt := FormatWithheldMarker(tc.input)
			if gotFmt != tc.wantFormat {
				t.Errorf("FormatWithheldMarker(%v) = %q, want %q", tc.input, gotFmt, tc.wantFormat)
			}
			gotParsed := ParseWithheldMarker(gotFmt)
			if !slices.Equal(gotParsed, tc.wantParsed) {
				t.Errorf("ParseWithheldMarker(%q) = %v, want %v", gotFmt, gotParsed, tc.wantParsed)
			}
		})
	}

	// Extra parsing edge cases: stray commas and spaces.
	if got := ParseWithheldMarker(" , B , A , B , "); !slices.Equal(got, []string{"A", "B"}) {
		t.Errorf("ParseWithheldMarker stray commas = %v, want [A B]", got)
	}
}
