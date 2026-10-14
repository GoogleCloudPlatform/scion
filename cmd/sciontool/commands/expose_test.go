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

package commands

import (
	"testing"

	"github.com/spf13/cobra"
)

// TestExposeCommandsMoved checks that sciontool expose and unexpose are
// hidden stubs that fail with a message naming the scion command, whatever
// arguments and flags they are given (ptone/scion#4728).
func TestExposeCommandsMoved(t *testing.T) {
	for _, name := range []string{"expose", "unexpose"} {
		t.Run(name, func(t *testing.T) {
			var stub *cobra.Command
			for _, c := range rootCmd.Commands() {
				if c.Name() == name {
					stub = c
				}
			}
			if stub == nil {
				t.Fatalf("sciontool %s is not registered", name)
			}
			if !stub.Hidden {
				t.Errorf("sciontool %s should be hidden", name)
			}
			if !stub.DisableFlagParsing {
				t.Errorf("sciontool %s should accept any flags", name)
			}
			err := stub.RunE(stub, []string{"8080", "--label", "x", "--list"})
			if err == nil {
				t.Fatalf("sciontool %s should fail", name)
			}
			want := "sciontool " + name + " has moved to 'scion " + name + "'"
			if err.Error() != want {
				t.Errorf("error = %q, want %q", err.Error(), want)
			}
		})
	}
}
