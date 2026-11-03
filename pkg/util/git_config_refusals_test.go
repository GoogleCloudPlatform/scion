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
)

// One seed check names every refused key of .git/config in one error, in
// order, each once, with the remedy; it never prints a value.
func TestPrepareSeededGitConfig_ReportsAllRefusedKeys(t *testing.T) {
	raw := "[core]\n\teditor = vi-valmarker-a1\n\tpager = less-valmarker-a2\n" +
		"[grep]\n\tthreads = 8\n\tthreads = 9\n" +
		"[user]\n\tname = ok\n"
	_, _, err := PrepareSeededGitConfig([]byte(raw), RepoPolicyLinked, nil)
	var many *RepoConfigRefusals
	if !errors.As(err, &many) {
		t.Fatalf("error = %v, want *RepoConfigRefusals", err)
	}
	var keys []string
	for _, r := range many.Refusals {
		keys = append(keys, r.Key)
	}
	if got, want := strings.Join(keys, ","), "core.editor,core.pager,grep.threads"; got != want {
		t.Fatalf("refused keys = %s, want %s (in order, each once)", got, want)
	}
	want := "agent git config refused: repository config sets 3 keys that are not allowed: core.editor, core.pager, grep.threads (remove each from the repository's .git/config)."
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
	_, _, err := PrepareSeededGitConfig([]byte("[core]\n\teditor = vi\n"), RepoPolicyLinked, nil)
	var many *RepoConfigRefusals
	if errors.As(err, &many) {
		t.Fatalf("one refusal returned as a list: %v", err)
	}
	want := "agent git config refused: repository config sets core.editor, which is not on the linked-checkout allow-list; remove it from the repository's .git/config"
	if err == nil || err.Error() != want {
		t.Errorf("text = %v, want %q", err, want)
	}
}

// Keys from different layers are grouped by their remedy and layer, and the
// note says the git checks are still to run.
func TestRepoConfigRefusals_GroupsByLayerWithNote(t *testing.T) {
	err := joinSeedRefusals([]*RepoConfigRefusedError{
		newRepoConfigRefusedError("", "", "core.editor", "v1", "r", true, AudienceRemote),
		newRepoConfigRefusedError("", seedLayerCommonWorktree, "core.pager", "v2", "r", true, AudienceRemote),
		newRepoConfigRefusedError("", "", "grep.threads", "v3", "r", true, AudienceRemote),
		newRepoConfigRefusedError("", seedLayerCommonWorktree, "core.pager", "v4", "r", true, AudienceRemote),
	}, true)
	want := "agent git config refused: repository config sets 3 keys that are not allowed: " +
		"core.editor, grep.threads (remove each from the repository's .git/config); " +
		"core.pager (remove each from the git config file that sets it, in config.worktree)." +
		" Checks that run git run once these are fixed and can report more."
	if err == nil || err.Error() != want {
		t.Errorf("text =\n  %v\nwant\n  %q", err, want)
	}
}

// The text fits the Hub's stored message bound: a long list is shortened
// with "and N more", never cut mid-key.
func TestRepoConfigRefusals_FitsStoredMessage(t *testing.T) {
	var refusals []*RepoConfigRefusedError
	for i := range 40 {
		refusals = append(refusals, newRepoConfigRefusedError("", "", fmt.Sprintf("branch.feature-%02d-with-a-long-name.description2", i), "", "r", true, AudienceRemote))
	}
	err := joinSeedRefusals(refusals, true)
	msg := err.Error()
	if len(msg) > maxRefusalsTextBytes {
		t.Fatalf("text is %d bytes, over %d: %s", len(msg), maxRefusalsTextBytes, msg)
	}
	if !strings.Contains(msg, "sets 40 keys") || !strings.Contains(msg, " more.") || !strings.Contains(msg, "branch.feature-00-") {
		t.Errorf("shortened text must keep the count, the first keys and \"and N more\": %s", msg)
	}
	if !strings.HasSuffix(msg, refusalsGitChecksNote) {
		t.Errorf("shortened text lost the note: %s", msg)
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
