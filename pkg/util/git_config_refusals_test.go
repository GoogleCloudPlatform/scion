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

package util

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"
)

// One seed check names every refused key of .git/config in one error, in
// order, each once, with the remedy; it never prints a value.
func TestPrepareSeededGitConfig_ReportsAllRefusedKeys(t *testing.T) {
	raw := "[core]\n\teditor = vi-valmarker-a1\n\tpager = less-valmarker-a2\n" +
		"[mytool]\n\tsetting = 8\n\tsetting = 9\n" +
		"[user]\n\tname = ok\n"
	_, _, _, err := PrepareSeededGitConfig([]byte(raw), RepoPolicyLinked, nil, false)
	var many *RepoConfigRefusals
	if !errors.As(err, &many) {
		t.Fatalf("error = %v, want *RepoConfigRefusals", err)
	}
	var keys []string
	for _, r := range many.Refusals {
		keys = append(keys, r.Key)
	}
	if got, want := strings.Join(keys, ","), "core.editor,core.pager,mytool.setting"; got != want {
		t.Fatalf("refused keys = %s, want %s (in order, each once)", got, want)
	}
	want := "agent git config refused: repository config sets 3 keys that are not allowed: " +
		"core.editor (a command or hook-location setting, never given to agents), " +
		"core.pager (a command or hook-location setting, never given to agents), " +
		"mytool.setting (not on the linked-checkout allow-list): remove each from the repository's .git/config."
	if got := err.Error(); got != want {
		t.Errorf("text =\n  %q\nwant\n  %q", got, want)
	}
	if many.GitChecksPending {
		t.Error("PrepareSeededGitConfig runs no git check, so none is pending")
	}
	// errors.As still finds the first single refusal for callers that look for one.
	var one *RepoConfigRefusedError
	if !errors.As(err, &one) || one.Key != "core.editor" {
		t.Errorf("errors.As single = %v, want the core.editor refusal", one)
	}
}

// One refused key keeps exactly the single-refusal type and text.
func TestPrepareSeededGitConfig_OneRefusalKeepsLegacyText(t *testing.T) {
	_, _, _, err := PrepareSeededGitConfig([]byte("[mytool]\n\tsetting = 1\n"), RepoPolicyLinked, nil, false)
	var many *RepoConfigRefusals
	if errors.As(err, &many) {
		t.Fatalf("one refusal returned as a list: %v", err)
	}
	want := "agent git config refused: repository config sets mytool.setting, which is not on the linked-checkout allow-list; remove it from the repository's .git/config"
	if err == nil || err.Error() != want {
		t.Errorf("text = %v, want %q", err, want)
	}
}

func refusalsFixture(n int, key func(int) string, layer func(int) string) []*RepoConfigRefusedError {
	var out []*RepoConfigRefusedError
	for i := range n {
		out = append(out, newRepoConfigRefusedError("", layer(i), key(i), "v", "rsnmarker, a reason long enough that eight of them do not fit the bound", true, AudienceRemote))
	}
	return out
}

// A list that fits keeps each key's reason; keys from different layers are
// grouped by remedy and layer, and the note names the skipped git checks.
func TestRepoConfigRefusals_ReasonsWhenTheListFits(t *testing.T) {
	err := joinSeedRefusals([]*RepoConfigRefusedError{
		newRepoConfigRefusedError("", "", "core.editor", "v1", "r1", true, AudienceRemote),
		newRepoConfigRefusedError("", seedLayerCommonWorktree, "core.pager", "v2", "r2", true, AudienceRemote),
		newRepoConfigRefusedError("", "", "grep.threads", "v3", "r3", true, AudienceRemote),
		newRepoConfigRefusedError("", seedLayerCommonWorktree, "core.pager", "v4", "r2", true, AudienceRemote),
	}, true)
	want := "agent git config refused: repository config sets 3 keys that are not allowed: " +
		"core.editor (r1), grep.threads (r3): remove each from the repository's .git/config; " +
		"core.pager (r2): remove each from the git config file that sets it, in config.worktree." +
		" The git-based checks (the cross-check and the submodule check) run once these are fixed and can report more."
	if err == nil || err.Error() != want {
		t.Errorf("text =\n  %v\nwant\n  %q", err, want)
	}
}

// A list that does not fit with reasons falls back to keys and remedies
// only, and keeps every key when that fits.
func TestRepoConfigRefusals_CompactWhenReasonsDoNotFit(t *testing.T) {
	err := joinSeedRefusals(refusalsFixture(8, func(i int) string { return fmt.Sprintf("grep.key%d", i) }, func(int) string { return "" }), false)
	msg := err.Error()
	want := "agent git config refused: repository config sets 8 keys that are not allowed: " +
		"grep.key0, grep.key1, grep.key2, grep.key3, grep.key4, grep.key5, grep.key6, grep.key7: remove each from the repository's .git/config."
	if msg != want {
		t.Errorf("text =\n  %q\nwant\n  %q", msg, want)
	}
	// The same list with reasons would not have fit.
	if full := err.(*RepoConfigRefusals).text(8, true, ""); len(full) <= maxRefusalsTextBytes {
		t.Fatalf("fixture: the list with reasons fits (%d bytes); make the reasons longer", len(full))
	}
}

// A long list is shortened with "and N more", never cut mid-key, and keeps
// the count and the note.
func TestRepoConfigRefusals_FitsStoredMessage(t *testing.T) {
	err := joinSeedRefusals(refusalsFixture(40, func(i int) string {
		return fmt.Sprintf("branch.feature-%02d-with-a-long-name.description2", i)
	}, func(int) string { return "" }), true)
	msg := err.Error()
	if len(msg) > maxRefusalsTextBytes {
		t.Fatalf("text is %d bytes, over %d: %s", len(msg), maxRefusalsTextBytes, msg)
	}
	if !strings.Contains(msg, "sets 40 keys") || !strings.Contains(msg, " more.") || !strings.Contains(msg, "branch.feature-00-") || strings.Contains(msg, "rsnmarker") {
		t.Errorf("shortened text must keep the count, the first keys and \"and N more\", without reasons: %s", msg)
	}
	if !strings.HasSuffix(msg, refusalsGitChecksNote) {
		t.Errorf("shortened text lost the note: %s", msg)
	}
}

// One key too long to fit alone is shortened with an ellipsis on a rune
// boundary, so the count and the note still survive.
func TestRepoConfigRefusals_LongKeyShortened(t *testing.T) {
	// Printed key names are plain ASCII (safeSubsectionLabel), so a long one
	// is a long plain subsection.
	long := "branch." + strings.Repeat("ab-", 300) + ".vscode-merge-base"
	err := joinSeedRefusals([]*RepoConfigRefusedError{
		newRepoConfigRefusedError("", "", long, "v", "rsnmarker", true, AudienceRemote),
		newRepoConfigRefusedError("", "", "core.editor", "v", "rsnmarker", true, AudienceRemote),
	}, true)
	msg := err.Error()
	if len(msg) > maxRefusalsTextBytes || !utf8.ValidString(msg) {
		t.Fatalf("text is %d bytes (max %d), valid UTF-8 %v: %q", len(msg), maxRefusalsTextBytes, utf8.ValidString(msg), msg)
	}
	if !strings.Contains(msg, "sets 2 keys") || !strings.Contains(msg, "\u2026: remove each") || !strings.Contains(msg, "; and 1 more.") || !strings.HasSuffix(msg, refusalsGitChecksNote) {
		t.Errorf("text must keep the count, the shortened key, \"and 1 more\" and the note: %q", msg)
	}
}

// No sink prints a refused value: every fmt verb, a wrapped error and slog
// print Error().
func TestRepoConfigRefusals_NoValue(t *testing.T) {
	err := joinSeedRefusals([]*RepoConfigRefusedError{
		newRepoConfigRefusedError("", "", "core.editor", "valmarker-b1", "r", true, AudienceRemote),
		newRepoConfigRefusedError("", "", "core.pager", "valmarker-b2", "r", true, AudienceRemote),
	}, false)
	many, ok := err.(*RepoConfigRefusals)
	if !ok {
		t.Fatalf("error = %T, want *RepoConfigRefusals", err)
	}
	want := err.Error()
	for sink, got := range refusalSinks(t, err) {
		if strings.Contains(got, "valmarker-b") {
			t.Errorf("%s prints a value: %q", sink, got)
		}
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if got := fmt.Sprintf(verb, *many); got != want {
			t.Errorf("Sprintf(%s, copy) = %q, want Error()", verb, got)
		}
	}
	if got := slog.AnyValue(*many).Resolve().String(); got != want {
		t.Errorf("LogValue = %q, want Error()", got)
	}
}

// StartRefusalText returns only a refusal's own text, and only one that names
// no host directory.
func TestStartRefusalText(t *testing.T) {
	seeded := newRepoConfigRefusedError("", "", "core.editor", "v", "r", true, AudienceRemote)
	many := joinSeedRefusals([]*RepoConfigRefusedError{seeded, newRepoConfigRefusedError("", "", "core.pager", "v", "r", true, AudienceRemote)}, false)
	for _, tc := range []struct {
		name string
		err  error
		want string
		ok   bool
	}{
		{"seeded, wrapped", fmt.Errorf("seed private git config for agent %q: %w", "a1", seeded), seeded.Error(), true},
		{"list, wrapped", fmt.Errorf("start: %w", many), many.Error(), true},
		{"host remote", fmt.Errorf("x: %w", newRepoConfigRefusedError("/srv/r", "", "core.editor", "v", "r", false, AudienceRemote)), "git in the project repository refused: repository config sets core.editor, which is r; remove it from the repository's .git/config (or config.worktree)", true},
		{"host local names a dir", newRepoConfigRefusedError("/srv/r", "", "core.editor", "v", "r", false, AudienceLocal), "", false},
		{"other", errors.New("boom"), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := StartRefusalText(tc.err)
			if got != tc.want || ok != tc.ok {
				t.Errorf("StartRefusalText = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// shortenKey cuts on a rune boundary and counts the ellipsis in the limit.
func TestShortenKey(t *testing.T) {
	for _, tc := range []struct {
		key   string
		limit int
		want  string
	}{
		{"core.editor", 20, "core.editor"},
		{"abcdefghij", 8, "abcde\u2026"},
		{"ab\u00e9\u00e9\u00e9\u00e9", 8, "ab\u00e9\u2026"}, // 10 bytes; a cut at byte 5 would split the second \u00e9
	} {
		got := shortenKey(tc.key, tc.limit)
		if got != tc.want || len(got) > max(tc.limit, len(tc.key)) || !utf8.ValidString(got) {
			t.Errorf("shortenKey(%q, %d) = %q, want %q", tc.key, tc.limit, got, tc.want)
		}
	}
}

// A nil or empty RepoConfigRefusals gives the generic text and no wrapped errors, instead of panicking.
func TestRepoConfigRefusals_NilAndEmpty(t *testing.T) {
	var nilList *RepoConfigRefusals
	for name, e := range map[string]*RepoConfigRefusals{"nil": nilList, "empty": {}, "nil slice": {Refusals: nil}} {
		if got := e.Error(); got != "agent git config refused" {
			t.Errorf("%s: Error() = %q, want the generic text", name, got)
		}
		if got := e.Unwrap(); len(got) != 0 {
			t.Errorf("%s: Unwrap() = %v, want none", name, got)
		}
	}
}
