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
	"errors"
	"strings"
	"testing"
)

func TestChooseOption_Terminal(t *testing.T) {
	options := []string{"foo on prod (https://hub.example.com)", "foo on staging (https://staging.example.com)"}
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{"first", "1\n", 0},
		{"second", "2\n", 1},
		{"answer without a newline at the end of input", "2", 1},
		{"invalid answers are asked again", "x\n3\n0\n2\n", 1},
		{"cancel", "c\n", -1},
		{"cancel spelled out", "Cancel\n", -1},
		{"end of input cancels", "", -1},
		{"invalid answer at the end of input cancels", "9", -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := withPromptIO(t, strings.NewReader(tt.input), true)
			got, err := ChooseOption("Project 'foo' exists on several hubs:", options)
			if err != nil {
				t.Fatalf("ChooseOption error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ChooseOption = %d, want %d", got, tt.want)
			}
			text := out.String()
			for _, line := range []string{
				"Project 'foo' exists on several hubs:\n",
				"  1) foo on prod (https://hub.example.com)\n",
				"  2) foo on staging (https://staging.example.com)\n",
				"Enter choice 1-2 (or 'c' to cancel): ",
			} {
				if !strings.Contains(text, line) {
					t.Errorf("prompt output %q does not contain %q", text, line)
				}
			}
		})
	}
}

func TestChooseOption_InvalidAnswerMessage(t *testing.T) {
	out := withPromptIO(t, strings.NewReader("7\n1\n"), true)
	if got, err := ChooseOption("Pick:", []string{"a", "b"}); err != nil || got != 0 {
		t.Fatalf("ChooseOption = %d, %v; want 0, nil", got, err)
	}
	if !strings.Contains(out.String(), "Invalid choice. Please enter 1-2.\n") {
		t.Errorf("no invalid-choice message in %q", out.String())
	}
}

func TestChooseOption_NoTerminal(t *testing.T) {
	out := withPromptIO(t, strings.NewReader("1\n"), false)
	got, err := ChooseOption("Pick:", []string{"a", "b"})
	if !errors.Is(err, ErrNoTerminal) {
		t.Fatalf("error = %v, want ErrNoTerminal", err)
	}
	if got != -1 {
		t.Errorf("index = %d, want -1", got)
	}
	if out.Len() != 0 {
		t.Errorf("nothing is printed without a terminal, got %q", out.String())
	}
}

func TestChooseOption_NoOptions(t *testing.T) {
	withPromptIO(t, strings.NewReader("1\n"), true)
	if got, err := ChooseOption("Pick:", nil); err == nil || got != -1 {
		t.Errorf("ChooseOption(nil) = %d, %v; want -1 and an error", got, err)
	}
}
