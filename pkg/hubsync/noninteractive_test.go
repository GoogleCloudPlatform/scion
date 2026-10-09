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

package hubsync

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// withPromptIO points the prompt helpers at in, a buffer for prompt output,
// and a fixed terminal answer for the duration of the test. It returns the
// buffer that receives prompt output.
func withPromptIO(t *testing.T, in io.Reader, isTTY bool) *bytes.Buffer {
	t.Helper()
	origIn, origOut, origTTY := promptIn, promptOut, stdinIsTerminal
	t.Cleanup(func() { promptIn, promptOut, stdinIsTerminal = origIn, origOut, origTTY })
	var out bytes.Buffer
	promptIn = in
	promptOut = &out
	stdinIsTerminal = func() bool { return isTTY }
	return &out
}

// idleStdin returns the read end of a pipe whose write end stays open and
// silent, like the stdin a coding agent's tool runner hands the CLI. Any
// read from it blocks until the test ends.
func idleStdin(t *testing.T) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(); _ = r.Close() })
	return r
}

// within fails the test if fn does not return within a short deadline.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked on an idle open stdin", what)
	}
}

func TestPrompts_IdleOpenStdin_DoNotHang(t *testing.T) {
	matches := []ProjectMatch{{ID: "id-1", Name: "widgets"}, {ID: "id-2", Name: "widgets"}}

	t.Run("ConfirmAction", func(t *testing.T) {
		out := withPromptIO(t, idleStdin(t), false)
		var got bool
		within(t, "ConfirmAction", func() { got = ConfirmAction("Link project with Hub?", true, false) })
		if got {
			t.Error("ConfirmAction without a terminal = true, want false")
		}
		if !strings.Contains(out.String(), "--yes") {
			t.Errorf("prompt output should name --yes, got %q", out.String())
		}
	})

	t.Run("ShowMatchingProjectsPrompt", func(t *testing.T) {
		withPromptIO(t, idleStdin(t), false)
		var err error
		within(t, "ShowMatchingProjectsPrompt", func() {
			_, _, err = ShowMatchingProjectsPrompt("widgets", matches, "", false, false)
		})
		if !errors.Is(err, ErrNoTerminal) {
			t.Fatalf("error = %v, want ErrNoTerminal", err)
		}
		if !strings.Contains(err.Error(), "--yes") {
			t.Errorf("error should name --yes, got %q", err)
		}
	})

	t.Run("ShowProjectLinkOrDisablePrompt", func(t *testing.T) {
		withPromptIO(t, idleStdin(t), false)
		var got LinkOrDisableChoice
		within(t, "ShowProjectLinkOrDisablePrompt", func() { got = ShowProjectLinkOrDisablePrompt("widgets", false) })
		if got != LinkOrDisableCancel {
			t.Errorf("choice = %v, want LinkOrDisableCancel", got)
		}
	})
}

// TestPrompts_AutoConfirmWritesNothingToStdout checks that auto-confirmed
// prompts, their context lines and the auto-confirm notes stay off stdout,
// which carries --format json output.
func TestPrompts_AutoConfirmWritesNothingToStdout(t *testing.T) {
	if promptOut != io.Writer(os.Stderr) {
		t.Fatalf("prompt output defaults to %v, want os.Stderr", promptOut)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	errOut := withPromptIO(t, strings.NewReader(""), false)
	ShowSyncPlan(&SyncResult{ToRegister: []string{"a1"}}, true)
	ShowLinkPrompt("widgets", true)
	ShowProjectDeletePrompt("widgets", 2, nil, true)
	_, _, _ = ShowMatchingProjectsPrompt("widgets", []ProjectMatch{{ID: "id-1", Name: "widgets"}}, "", true, false)
	ShowProjectLinkOrDisablePrompt("widgets", true)

	_ = w.Close()
	os.Stdout = origStdout
	stdout, _ := io.ReadAll(r)
	if len(stdout) != 0 {
		t.Errorf("auto-confirmed prompts wrote to stdout: %q", stdout)
	}
	for _, want := range []string{"auto-confirmed Yes", "Auto-linking to: widgets", "Auto-selecting"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("prompt output missing %q:\n%s", want, errOut.String())
		}
	}
}

func TestShowMatchingProjectsPrompt_NonInteractiveAmbiguous(t *testing.T) {
	matches := []ProjectMatch{
		{ID: "id-1", Name: "widgets", Slug: "widgets"},
		{ID: "id-2", Name: "widgets", Slug: "widgets-2"},
	}
	withPromptIO(t, strings.NewReader("1\n"), true)

	// --non-interactive implies --yes, so autoConfirm is true as well.
	choice, id, err := ShowMatchingProjectsPrompt("widgets", matches, "widgets-3", true, true)
	if !errors.Is(err, ErrAmbiguousProject) {
		t.Fatalf("error = %v, want ErrAmbiguousProject", err)
	}
	if choice != ProjectChoiceCancel || id != "" {
		t.Errorf("got (%v, %q), want (ProjectChoiceCancel, \"\")", choice, id)
	}
	for _, want := range []string{"id-1", "id-2", "--non-interactive"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestShowMatchingProjectsPrompt_NonInteractiveSingleMatchLinks(t *testing.T) {
	withPromptIO(t, strings.NewReader(""), false)
	choice, id, err := ShowMatchingProjectsPrompt("widgets", []ProjectMatch{{ID: "id-1", Name: "widgets"}}, "", true, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if choice != ProjectChoiceLink || id != "id-1" {
		t.Errorf("got (%v, %q), want (ProjectChoiceLink, \"id-1\")", choice, id)
	}
}

func TestShowMatchingProjectsPrompt_TerminalChoice(t *testing.T) {
	matches := []ProjectMatch{{ID: "id-1", Name: "widgets"}, {ID: "id-2", Name: "widgets"}}
	withPromptIO(t, strings.NewReader("2\n"), true)
	choice, id, err := ShowMatchingProjectsPrompt("widgets", matches, "", false, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if choice != ProjectChoiceLink || id != "id-2" {
		t.Errorf("got (%v, %q), want (ProjectChoiceLink, \"id-2\")", choice, id)
	}
}

// An empty matches list must not index matches[0]: there is nothing to link
// to, so every mode returns the register-new choice.
func TestShowMatchingProjectsPrompt_NoMatchesRegistersNew(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		autoConfirm, nonInteractive bool
		isTTY                       bool
	}{
		{"autoConfirm", true, false, false},
		{"nonInteractive", true, true, false},
		{"noTerminal", false, false, false},
		{"terminal", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withPromptIO(t, strings.NewReader(""), tc.isTTY)
			choice, id, err := ShowMatchingProjectsPrompt("widgets", nil, "", tc.autoConfirm, tc.nonInteractive)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if choice != ProjectChoiceRegisterNew || id != "" {
				t.Errorf("got (%v, %q), want (ProjectChoiceRegisterNew, \"\")", choice, id)
			}
		})
	}
}
