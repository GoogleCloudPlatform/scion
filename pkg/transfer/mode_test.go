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

package transfer

import (
	"os"
	"testing"
)

func TestNormalizeMode(t *testing.T) {
	tests := map[string]os.FileMode{
		"0755":  ModeExecutable,
		"0700":  ModeExecutable,
		"0775":  ModeExecutable,
		"0744":  ModeExecutable,
		"0645":  ModeExecutable, // other-exec only still counts
		"4755":  ModeExecutable, // setuid dropped
		"6711":  ModeExecutable, // setuid+setgid dropped
		"1777":  ModeExecutable, // sticky dropped
		"4644":  ModeRegular,    // setuid on a non-exec file dropped
		"0644":  ModeRegular,
		"0600":  ModeRegular,
		"0666":  ModeRegular, // group/other write dropped
		"":      ModeRegular, // legacy entry with no mode
		"junk":  ModeRegular,
		"0999":  ModeRegular,
		"-0755": ModeRegular,
	}
	for in, want := range tests {
		if got := NormalizeMode(in); got != want {
			t.Errorf("NormalizeMode(%q) = %v, want %v", in, got, want)
		}
	}
	if got := NormalizedModeString("4755"); got != "0755" {
		t.Errorf("NormalizedModeString(4755) = %q", got)
	}
	if got := NormalizedModeString(""); got != "0644" {
		t.Errorf("NormalizedModeString(\"\") = %q", got)
	}
}
